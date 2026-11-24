package tender

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// EngineConfig binds an adapter to one host merchant, environment, processor
// account and market. Configure the adapter with matching credentials. Now and
// NewID may be provided for deterministic simulations and must be concurrency-safe.
type EngineConfig struct {
	Store                         Store
	Provider                      Provider
	Scope                         Scope
	ProviderName, MerchantAccount string
	Market                        Market
	Now                           func() time.Time
	NewID                         func() string
	// Timeout bounds each provider call (including all recovery query attempts).
	// Zero uses 15 seconds. An earlier caller deadline always takes precedence.
	Timeout time.Duration
}

type Engine struct {
	config EngineConfig
	caps   Capabilities
}

func NewEngine(c EngineConfig) (*Engine, error) {
	if err := c.Scope.Validate(); err != nil {
		return nil, err
	}
	if c.Store == nil || c.Provider == nil || strings.TrimSpace(c.ProviderName) == "" ||
		strings.TrimSpace(c.MerchantAccount) == "" || len(c.Market.Country) != 2 ||
		c.Market.Method == "" || c.Timeout < 0 {
		return nil, fmt.Errorf("%w: invalid engine configuration", ErrValidation)
	}
	if _, err := CurrencyExponent(c.Market.Currency); err != nil {
		return nil, err
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.NewID == nil {
		c.NewID = randomID
	}
	if c.Timeout == 0 {
		c.Timeout = 15 * time.Second
	}
	return &Engine{config: c, caps: c.Provider.Capabilities()}, nil
}

func randomID() string {
	var b [16]byte
	crand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Request carries trusted host values. Supply OrderID only for Create and
// specify the amount for every mutation. A void covers the remaining
// authorization or cancels a pending session for its full amount. Token is an opaque, transient adapter input. Exact replay
// requires the original request, including the token.
type Request struct {
	PaymentID, OrderID, Key string
	Amount                  Money
	Token                   string
}

// Result pairs the current payment with the specific requested operation.
// An identical concurrent caller may observe a reservation still in progress.
// Only the caller that committed a new reservation executes the mutation.
type Result struct {
	Payment   Payment
	Operation Operation
}

func (e *Engine) Create(ctx context.Context, r Request) (Result, error) {
	return e.execute(ctx, OpCreate, r)
}

func (e *Engine) Confirm(ctx context.Context, r Request) (Result, error) {
	return e.execute(ctx, OpConfirm, r)
}

func (e *Engine) Capture(ctx context.Context, r Request) (Result, error) {
	return e.execute(ctx, OpCapture, r)
}

func (e *Engine) Void(ctx context.Context, r Request) (Result, error) {
	return e.execute(ctx, OpVoid, r)
}

func (e *Engine) Refund(ctx context.Context, r Request) (Result, error) {
	return e.execute(ctx, OpRefund, r)
}

func (e *Engine) route(p Payment) error {
	if p.Scope != e.config.Scope || p.Provider != e.config.ProviderName || p.MerchantAccount != e.config.MerchantAccount || p.Market != e.config.Market {
		return fmt.Errorf("%w: payment belongs to another route", ErrConflict)
	}
	return nil
}

// Get reads the payment in the configured route.
func (e *Engine) Get(ctx context.Context, id string) (Payment, error) {
	var p Payment
	err := e.config.Store.Transact(ctx, func(tx Tx) error {
		var err error
		p, err = tx.Payment(e.config.Scope, id)
		if err != nil {
			return err
		}
		return e.route(p)
	})
	if err != nil {
		return Payment{}, err
	}
	return p, nil
}

func (e *Engine) fingerprint(kind OperationKind, r Request) string {
	h := sha256.New()
	for _, s := range []string{
		"tender-request-v1", e.config.Scope.MerchantID, string(e.config.Scope.Environment),
		e.config.ProviderName, e.config.MerchantAccount, e.config.Market.Country,
		e.config.Market.Currency, string(e.config.Market.Method), string(kind),
		r.PaymentID, r.OrderID, r.Key, r.Amount.Currency, r.Token,
	} {
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(s))))
		h.Write([]byte(s))
	}
	h.Write(binary.BigEndian.AppendUint64(nil, uint64(r.Amount.Minor)))
	return hex.EncodeToString(h.Sum(nil))
}

type state struct {
	payment Payment
	op      OperationRecord
}

func (s state) result() (Result, error) {
	r := Result{Payment: s.payment, Operation: s.op.Operation}
	if s.op.Status == OperationUnknown {
		return r, ErrUnknownOutcome
	}
	if s.op.Failure != "" {
		return r, &ProviderError{Kind: s.op.Failure, ProviderStatus: s.op.FailureStatus, RetryAfter: s.op.RetryAfter}
	}
	return r, nil
}

func (e *Engine) execute(ctx context.Context, kind OperationKind, r Request) (Result, error) {
	s, dispatch, err := e.reserve(ctx, kind, r)
	if err != nil {
		return Result{}, err
	}
	if !dispatch {
		return s.result()
	}
	call, cancel := context.WithTimeout(ctx, e.config.Timeout)
	obs, callErr := e.config.Provider.Execute(call, ProviderRequest{Payment: s.payment, Operation: s.op.Operation, Token: r.Token})
	cancel()
	// A disconnected host request must not discard a provider result.
	finish, stop := context.WithTimeout(context.WithoutCancel(ctx), e.config.Timeout)
	defer stop()
	if callErr != nil {
		return e.finishError(finish, s, callErr)
	}
	final, err := e.apply(finish, s, obs, "response")
	if err != nil {
		// A malformed or unbound reply, or a failed local commit, does not
		// establish that the remote mutation failed. Leave it recoverable.
		result, uncertain := e.finishError(finish, s, ErrUnknownOutcome)
		if uncertain == nil {
			return result, nil
		}
		return result, errors.Join(err, uncertain)
	}
	return final.result()
}

func (e *Engine) reserve(ctx context.Context, kind OperationKind, r Request) (state, bool, error) {
	if strings.TrimSpace(r.Key) == "" || strings.TrimSpace(r.PaymentID) == "" {
		return state{}, false, fmt.Errorf("%w: missing payment or operation key", ErrValidation)
	}
	hash := e.fingerprint(kind, r)
	var s state
	var dispatch bool
	err := e.config.Store.Transact(ctx, func(tx Tx) error {
		existing, err := tx.OperationByKey(e.config.Scope, r.Key)
		if err == nil {
			if existing.Fingerprint != hash {
				return fmt.Errorf("%w: changed request under existing key", ErrConflict)
			}
			p, err := tx.Payment(e.config.Scope, existing.PaymentID)
			if err != nil {
				return err
			}
			s = state{p, existing}
			return e.route(p)
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		var p Payment
		if kind == OpCreate {
			p, err = NewPayment(e.config.Scope, r.PaymentID, r.OrderID, r.Amount)
			if err != nil {
				return err
			}
			p.Provider, p.MerchantAccount, p.Market = e.config.ProviderName, e.config.MerchantAccount, e.config.Market
			p.AttemptID = e.config.NewID()
		} else {
			if r.OrderID != "" {
				return fmt.Errorf("%w: unexpected order ID", ErrValidation)
			}
			p, err = tx.Payment(e.config.Scope, r.PaymentID)
			if err != nil {
				return err
			}
			if err = e.route(p); err != nil {
				return err
			}
			if _, err = tx.ActiveOperation(p.Scope, p.ID); err == nil {
				return fmt.Errorf("%w: operation is in progress or requires recovery", ErrConflict)
			} else if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		if err := e.checkAmount(kind, r.Amount, p); err != nil {
			return err
		}
		op := Operation{
			Scope: p.Scope, PaymentID: p.ID, AttemptID: p.AttemptID, ID: e.config.NewID(),
			Key: r.Key, ProviderKey: e.config.NewID(), Fingerprint: hash, Amount: r.Amount,
			Kind: kind, BaseVersion: p.Version, Status: OperationReserved,
		}
		if p.AttemptID == "" || op.ID == "" || op.ProviderKey == "" {
			return fmt.Errorf("%w: missing generated identity", ErrValidation)
		}
		if kind == OpCreate {
			if err := tx.InsertPayment(p); err != nil {
				return err
			}
		}
		s = state{p, OperationRecord{Operation: op, PaymentVersion: p.Version}}
		if err := tx.InsertOperation(s.op); err != nil {
			return err
		}
		dispatch = true
		return e.audit(tx, s, Change{}, "reservation")
	})
	return s, dispatch, err
}

func (e *Engine) checkAmount(kind OperationKind, amount Money, p Payment) error {
	if err := amount.ValidatePurchase(); err != nil {
		return err
	}
	if amount.Currency != p.Amount.Currency || amount.Currency != p.Market.Currency {
		return fmt.Errorf("%w: different currency", ErrValidation)
	}
	f := p.Funds
	remaining := p.Amount.Minor
	switch kind {
	case OpConfirm:
		if !awaitingCustomer(p.Status) {
			return fmt.Errorf("%w: payment is not awaiting confirmation", ErrConflict)
		}
	case OpCapture, OpVoid:
		if kind == OpVoid && awaitingCustomer(p.Status) {
			break
		}
		if p.Status != PaymentAuthorized && p.Status != PaymentPartiallyCaptured {
			return fmt.Errorf("%w: no open authorization", ErrConflict)
		}
		remaining = f.Authorized - f.Captured
	case OpRefund:
		remaining = f.Captured - f.Refunded
	}
	partial := amount.Minor < remaining
	if amount.Minor > remaining || partial && kind != OpCapture && kind != OpRefund {
		return fmt.Errorf("%w: operation amount exceeds or differs from available principal", ErrValidation)
	}
	if !e.caps.SupportsIn(kind, p.Market, partial) {
		return ErrUnsupported
	}
	return nil
}

func (e *Engine) audit(tx Tx, s state, change Change, source string) error {
	at := change.At
	if at.IsZero() {
		at = e.config.Now().UTC()
	}
	return tx.AppendAudit(AuditEntry{
		Scope: s.payment.Scope, PaymentID: s.payment.ID, OperationID: s.op.ID,
		OperationStatus: s.op.Status, At: at, Source: source, Change: change,
	})
}

func (e *Engine) load(tx Tx, id string) (state, error) {
	op, err := tx.Operation(e.config.Scope, id)
	if err != nil {
		return state{}, err
	}
	p, err := tx.Payment(e.config.Scope, op.PaymentID)
	if err != nil {
		return state{}, err
	}
	return state{p, op}, e.route(p)
}

func (e *Engine) finishError(ctx context.Context, original state, cause error) (Result, error) {
	var final state
	err := e.config.Store.Transact(ctx, func(tx Tx) error {
		s, err := e.load(tx, original.op.ID)
		if err != nil {
			return err
		}
		final = s
		if s.op.Status != OperationReserved || s.payment.Version != original.payment.Version {
			return nil
		}
		next := OperationUnknown
		for _, kind := range []ErrorKind{ErrDeclined, ErrValidation, ErrAuthentication, ErrUnsupported, ErrRateLimit} {
			if !errors.Is(cause, ErrUnknownOutcome) && errors.Is(cause, kind) {
				s.op.Failure = kind
				var providerError *ProviderError
				if errors.As(cause, &providerError) {
					s.op.FailureStatus = sanitizeStatus(providerError.ProviderStatus)
					s.op.RetryAfter = max(0, providerError.RetryAfter)
				}
				next = OperationFailed
				break
			}
		}
		if err := s.op.SetStatus(next); err != nil {
			return err
		}
		if err := tx.UpdateOperation(s.op); err != nil {
			return err
		}
		final = s
		return e.audit(tx, s, Change{}, "dispatch_error")
	})
	if err != nil {
		return Result{original.payment, original.op.Operation}, errors.Join(ErrUnknownOutcome, err)
	}
	return final.result()
}

// ApplyVerified consumes a normalized observation from an authenticated
// provider callback. The adapter/inbox verifies and durably deduplicates its
// event first. A missing operation ID needs an already bound operation
// reference. Unresolved or out-of-order events require authoritative recovery.
func (e *Engine) ApplyVerified(ctx context.Context, o Observation) (Result, error) {
	s, err := e.apply(ctx, state{}, o, "webhook")
	if err != nil {
		return Result{}, err
	}
	return Result{s.payment, s.op.Operation}, nil
}

func (e *Engine) apply(ctx context.Context, original state, o Observation, source string) (state, error) {
	var final state
	err := e.config.Store.Transact(ctx, func(tx Tx) error {
		id := original.op.ID
		if source == "webhook" {
			if o.Scope != e.config.Scope || o.MerchantAccount != e.config.MerchantAccount {
				return fmt.Errorf("%w: wrong callback account", ErrConflict)
			}
			id = o.OperationID
			if id == "" {
				rec, err := tx.OperationByReference(o.Scope, o.PaymentID, o.OperationReference)
				if err != nil {
					return err
				}
				id = rec.ID
			}
		}
		s, err := e.load(tx, id)
		if err != nil {
			return err
		}
		if err := bind(s, o); err != nil {
			return err
		}
		final = s
		// A callback or later query won the race while the HTTP call was out.
		if source != "webhook" && s.payment.Version != original.payment.Version || source == "response" && s.op != original.op {
			return nil
		}
		if s.op.PaymentVersion != s.payment.Version {
			return fmt.Errorf("%w: stale observation for an earlier payment version", ErrConflict)
		}
		if active, err := tx.ActiveOperation(s.payment.Scope, s.payment.ID); err == nil && active.ID != s.op.ID {
			return fmt.Errorf("%w: another operation is in progress", ErrConflict)
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		o.Snapshot = o.Snapshot.principalSnapshot(s.payment.Status)
		o.Snapshot.ProviderStatus = sanitizeStatus(o.Snapshot.ProviderStatus)
		o.Snapshot.Action.ExpiresAt = o.Snapshot.Action.ExpiresAt.UTC()
		if err := validateOutcome(s, o, source); err != nil {
			return err
		}
		if s.op.Status == o.Status && s.payment.Snapshot == o.Snapshot &&
			(o.ProviderReference == "" || s.payment.ProviderReference == o.ProviderReference) &&
			(o.OperationReference == "" || s.op.ProviderReference == o.OperationReference) {
			return nil
		}

		if err := s.op.SetStatus(o.Status); err != nil {
			return err
		}
		if o.Status == OperationFailed && s.op.Failure == "" {
			s.op.Failure = ErrOperationFailed
			s.op.FailureStatus = o.Snapshot.ProviderStatus
			if o.Snapshot.Status == PaymentDeclined {
				s.op.Failure = ErrDeclined
			}
		}
		if o.ProviderReference != "" {
			s.payment.ProviderReference = o.ProviderReference
		}
		if o.OperationReference != "" {
			s.op.ProviderReference = o.OperationReference
		}
		change, err := s.payment.Apply(o.Snapshot, e.config.Now().UTC())
		if err != nil {
			return err
		}
		s.op.PaymentVersion = s.payment.Version
		if err := tx.UpdatePayment(s.payment); err != nil {
			return err
		}
		if err := tx.UpdateOperation(s.op); err != nil {
			return err
		}
		if err := e.audit(tx, s, change, source); err != nil {
			return err
		}
		f := s.payment.Funds
		if o.Status == OperationSucceeded && max(f.Captured, f.Settled) == s.payment.Amount.Minor && f.Returned == 0 && f.Refunded == 0 && f.Disputed == 0 {
			entry := OutboxEntry{
				Scope: s.payment.Scope, ID: s.payment.ID, PaymentID: s.payment.ID, OrderID: s.payment.OrderID,
				OperationID: s.op.ID, Version: s.payment.Version, At: change.At,
			}
			if err := tx.Enqueue(entry); err != nil {
				return err
			}
		}
		final = s
		return nil
	})
	return final, err
}

// ErrOperationFailed is a definitive unsuccessful operation without a more
// specific provider error category.
const ErrOperationFailed ErrorKind = "operation_failed"

func bind(s state, o Observation) error {
	p, op := s.payment, s.op
	if o.Scope != p.Scope || o.MerchantAccount != p.MerchantAccount || o.PaymentID != p.ID || o.Amount != op.Amount ||
		o.AttemptID != "" && o.AttemptID != op.AttemptID || o.OperationID != "" && o.OperationID != op.ID ||
		o.ProviderReference != "" && p.ProviderReference != "" && o.ProviderReference != p.ProviderReference ||
		o.OperationReference != "" && op.ProviderReference != "" && o.OperationReference != op.ProviderReference {
		return fmt.Errorf("%w: provider observation identity or amount mismatch", ErrConflict)
	}
	return nil
}

func awaitingCustomer(status PaymentStatus) bool {
	return status == PaymentPending || status == PaymentRequiresAction
}

func validateOutcome(s state, o Observation, source string) error {
	if o.Status != OperationPending && o.Status != OperationSucceeded && o.Status != OperationFailed {
		return fmt.Errorf("%w: unsupported provider outcome", ErrValidation)
	}
	p, op := s.payment, s.op
	f, base := o.Snapshot.Funds, p.Funds
	terminal := op.Status == OperationFailed || op.Status == OperationSucceeded
	// Creating a session may succeed before the provider collects the money.
	// Capture, refund and void reservations each authorize exactly one change.
	session := op.Kind == OpCreate || op.Kind == OpConfirm
	principalChanged := f.Authorized != base.Authorized || f.Captured != base.Captured || f.Refunded != base.Refunded
	if terminal && (op.Status != o.Status || !session && principalChanged) {
		return fmt.Errorf("%w: conflicting terminal observation", ErrConflict)
	}
	if o.Status == OperationFailed && (principalChanged || f.Settled != base.Settled) {
		return fmt.Errorf("%w: failed operation reports new funds", ErrConflict)
	}
	if session && !terminal && o.Status == OperationSucceeded && (o.Snapshot.Status == PaymentDeclined || o.Snapshot.Status == PaymentFailed) {
		return fmt.Errorf("%w: inconsistent session outcome", ErrConflict)
	}
	pendingUpdate := op.Status == OperationPending && o.Status == OperationPending
	unorderedAction := awaitingCustomer(p.Status) && awaitingCustomer(o.Snapshot.Status)
	if source == "webhook" && (pendingUpdate || unorderedAction) && p.Snapshot != o.Snapshot {
		return fmt.Errorf("%w: query required to order pending observations", ErrConflict)
	}
	if terminal {
		return nil
	}
	expectedDelta := int64(0)
	if o.Status == OperationSucceeded {
		expectedDelta = op.Amount.Minor
	}
	switch op.Kind {
	case OpCapture:
		if f.Authorized != base.Authorized || f.Captured-base.Captured != expectedDelta || f.Refunded != base.Refunded {
			return fmt.Errorf("%w: capture differs from reservation", ErrConflict)
		}
	case OpRefund:
		if f.Refunded-base.Refunded != expectedDelta || f.Captured != base.Captured || f.Authorized != base.Authorized {
			return fmt.Errorf("%w: refund differs from reservation", ErrConflict)
		}
	case OpVoid:
		if f.Captured != base.Captured || f.Refunded != base.Refunded || f.Authorized != base.Authorized ||
			o.Status == OperationSucceeded && o.Snapshot.Status != PaymentVoided {
			return fmt.Errorf("%w: void differs from reservation", ErrConflict)
		}
	}
	return nil
}

// Recover asks the adapter for an authoritative result using the persisted
// reservation and original provider key. Recovery never executes a new host
// mutation. Only the read-only query retries on rate limiting/transport errors.
func (e *Engine) Recover(ctx context.Context, operationID string) (Result, error) {
	var original state
	err := e.config.Store.Transact(ctx, func(tx Tx) error {
		var err error
		original, err = e.load(tx, operationID)
		return err
	})
	if err != nil {
		return Result{}, err
	}
	if !e.caps.Query {
		return Result{original.payment, original.op.Operation}, ErrUnsupported
	}
	call, cancel := context.WithTimeout(ctx, e.config.Timeout)
	defer cancel()
	req := ProviderRequest{Payment: original.payment, Operation: original.op.Operation}
	for attempt := 0; ; attempt++ {
		obs, err := e.config.Provider.Query(call, req)
		if err == nil {
			finish, stop := context.WithTimeout(context.WithoutCancel(ctx), e.config.Timeout)
			defer stop()
			s, err := e.apply(finish, original, obs, "query")
			if err != nil {
				return Result{}, err
			}
			return s.result()
		}
		last := Result{original.payment, original.op.Operation}
		if attempt == 2 || !errors.Is(err, ErrRateLimit) && !errors.Is(err, ErrTransport) {
			return last, err
		}
		backoff := time.Duration(1<<attempt) * 20 * time.Millisecond
		delay := backoff/2 + rand.N(backoff)
		var pe *ProviderError
		if errors.As(err, &pe) {
			delay = max(delay, pe.RetryAfter)
		}
		select {
		case <-call.Done():
			return last, call.Err()
		case <-time.After(delay):
		}
	}
}
