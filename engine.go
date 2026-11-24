package tender

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
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
	return e.execute(ctx, OpCreate, r, "")
}

// RetryCreate reserves a new create operation on the original payment and order
// after the adapter definitively rejected an earlier request without effect.
// Supply the rejected operation ID, a new key and the same trusted order and
// amount. A rejected operation has at most one successor. A retry before the
// provider's deadline returns ErrRateLimit with the remaining interval.
func (e *Engine) RetryCreate(ctx context.Context, rejectedOperationID string, r Request) (Result, error) {
	if strings.TrimSpace(rejectedOperationID) == "" {
		return Result{}, fmt.Errorf("%w: missing rejected operation ID", ErrValidation)
	}
	return e.execute(ctx, OpCreate, r, rejectedOperationID)
}

func (e *Engine) Confirm(ctx context.Context, r Request) (Result, error) {
	return e.execute(ctx, OpConfirm, r, "")
}

func (e *Engine) Capture(ctx context.Context, r Request) (Result, error) {
	return e.execute(ctx, OpCapture, r, "")
}

func (e *Engine) Void(ctx context.Context, r Request) (Result, error) {
	return e.execute(ctx, OpVoid, r, "")
}

func (e *Engine) Refund(ctx context.Context, r Request) (Result, error) {
	return e.execute(ctx, OpRefund, r, "")
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

func (e *Engine) fingerprint(kind OperationKind, r Request, predecessor string) string {
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
	if predecessor != "" {
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(predecessor))))
		h.Write([]byte(predecessor))
	}
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

func (e *Engine) execute(ctx context.Context, kind OperationKind, r Request, predecessor string) (Result, error) {
	s, dispatch, err := e.reserve(ctx, kind, r, predecessor)
	if err != nil {
		return Result{}, err
	}
	if !dispatch {
		return s.result()
	}
	timeout := e.config.Timeout
	if !s.op.DispatchUntil.IsZero() {
		timeout = min(timeout, s.op.DispatchUntil.Sub(e.config.Now().UTC()))
	}
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := call.Err(); err != nil {
		return e.finishDispatch(ctx, s, Observation{}, ErrNotDispatched)
	}
	req := ProviderRequest{Payment: s.payment, Operation: s.op.Operation, Token: r.Token, checkpoint: &dispatchCheckpoint{ctx: call, engine: e, owner: &s}}
	obs, callErr := e.config.Provider.Execute(call, req)
	return e.finishDispatch(ctx, s, obs, callErr)
}

// A checkpoint is valid only during this owner's Execute call. The transaction
// commits before the adapter is permitted to issue its financial POST.
type dispatchCheckpoint struct {
	ctx    context.Context
	engine *Engine
	owner  *state
}

func (c *dispatchCheckpoint) bind(digest [32]byte) error {
	original := *c.owner
	next := original.op
	next.ProviderEvidenceDigest = digest
	next.AwaitingEvidence = false
	err := c.engine.config.Store.Transact(c.ctx, func(tx Tx) error {
		current, err := c.engine.load(tx, original.op.ID)
		if err != nil {
			return err
		}
		if current.op != original.op || current.op.Status != OperationReserved || current.payment.Version != original.payment.Version || !current.op.DispatchUntil.IsZero() && !c.engine.config.Now().Before(current.op.DispatchUntil) {
			return ErrConflict
		}
		return tx.UpdateOperation(next)
	})
	if err == nil {
		c.owner.op = next
	}
	return err
}

func (e *Engine) finishDispatch(ctx context.Context, s state, obs Observation, callErr error) (Result, error) {
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

func (e *Engine) reserve(ctx context.Context, kind OperationKind, r Request, predecessor string) (state, bool, error) {
	if strings.TrimSpace(r.Key) == "" || strings.TrimSpace(r.PaymentID) == "" {
		return state{}, false, fmt.Errorf("%w: missing payment or operation key", ErrValidation)
	}
	hash := e.fingerprint(kind, r, predecessor)
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
		var rejected OperationRecord
		if predecessor != "" {
			previous, err := e.retryableCreate(tx, predecessor, r)
			if err != nil {
				return err
			}
			p, rejected = previous.payment, previous.op
		} else if kind == OpCreate {
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
		if err := e.checkStore(kind, tx); err != nil {
			return err
		}
		op := Operation{
			Scope: p.Scope, PaymentID: p.ID, AttemptID: p.AttemptID, ID: e.config.NewID(),
			Key: r.Key, ProviderKey: e.config.NewID(), Fingerprint: hash, Amount: r.Amount,
			Kind: kind, BaseVersion: p.Version, Status: OperationReserved,
		}
		if provider, ok := e.config.Provider.(IdempotentReplayProvider); ok {
			id, window := provider.ReplayIdentity(ProviderRequest{Payment: p, Operation: op})
			if id != "" && window > 0 {
				op.ReplayFingerprint = id
				op.ReplayUntil = e.config.Now().UTC().Add(window)
			}
		}
		if p.AttemptID == "" || op.ID == "" || op.ProviderKey == "" {
			return fmt.Errorf("%w: missing generated identity", ErrValidation)
		}
		if predecessor != "" {
			rejected.SuccessorID = op.ID
			if err := tx.UpdateOperation(rejected); err != nil {
				return err
			}
		} else if kind == OpCreate {
			if err := tx.InsertPayment(p); err != nil {
				return err
			}
		}
		s = state{p, OperationRecord{Operation: op, PaymentVersion: p.Version, AwaitingEvidence: slices.Contains(e.caps.BindBeforeDispatch, kind)}}
		if op.ReplayFingerprint != "" || e.caps.QueryAfterDispatch || s.op.AwaitingEvidence {
			s.op.DispatchUntil = e.config.Now().UTC().Add(e.config.Timeout)
		}
		if err := tx.InsertOperation(s.op); err != nil {
			return err
		}
		dispatch = true
		return e.audit(tx, s, Change{}, "reservation")
	})
	return s, dispatch, err
}

func (e *Engine) checkStore(kind OperationKind, tx Tx) error {
	if requirements, ok := e.config.Provider.(StoreRequirements); ok {
		return requirements.CheckStore(kind, tx)
	}
	return nil
}

func (e *Engine) retryableCreate(tx Tx, predecessor string, r Request) (state, error) {
	s, err := e.load(tx, predecessor)
	if err != nil {
		return state{}, err
	}
	p, op := s.payment, s.op
	if p.ID != r.PaymentID || p.OrderID != r.OrderID || p.Amount != r.Amount ||
		p.Status != PaymentCreated || p.Version != op.BaseVersion || p.ProviderReference != "" ||
		op.Kind != OpCreate || op.Status != OperationFailed || op.SuccessorID != "" {
		return state{}, fmt.Errorf("%w: create is not eligible for another attempt", ErrConflict)
	}
	switch op.Failure {
	case ErrRateLimit, ErrAuthentication, ErrValidation, ErrUnsupported, ErrNotDispatched:
		// These adapter errors are definitive rejections before any remote effect.
	default:
		return state{}, fmt.Errorf("%w: create did not receive a definite no-effect rejection", ErrConflict)
	}
	if delay := op.RetryNotBefore.Sub(e.config.Now()); delay > 0 {
		return state{}, &ProviderError{Kind: ErrRateLimit, ProviderStatus: op.FailureStatus, RetryAfter: delay}
	}
	return s, nil
}

func (e *Engine) checkAmount(kind OperationKind, amount Money, p Payment) error {
	if p.Status == PaymentCaptureFailed {
		return ErrConflict
	}
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
	if err := e.route(p); err != nil {
		return state{}, err
	}
	return state{p, op}, nil
}

func definitiveFailure(cause error) ErrorKind {
	if errors.Is(cause, ErrUnknownOutcome) || errors.Is(cause, ErrTransport) ||
		errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, context.Canceled) {
		return ""
	}
	for _, kind := range []ErrorKind{ErrDeclined, ErrValidation, ErrAuthentication, ErrUnsupported, ErrRateLimit, ErrNotDispatched} {
		if errors.Is(cause, kind) {
			return kind
		}
	}
	return ""
}

func (e *Engine) finishError(ctx context.Context, original state, cause error) (Result, error) {
	var final state
	err := e.config.Store.Transact(ctx, func(tx Tx) error {
		s, err := e.load(tx, original.op.ID)
		if err != nil {
			return err
		}
		final = s
		if s.op.Status != original.op.Status || !s.op.DispatchUntil.Equal(original.op.DispatchUntil) {
			return nil
		}
		if s.op.Status == OperationUnknown && original.op.ReplayFingerprint != "" {
			// A replay error describes only its own call. A prior dispatch may
			// have succeeded. Release the lease and retain the unknown outcome.
			s.op.DispatchUntil = time.Time{}
			final = s
			return tx.UpdateOperation(s.op)
		}
		if s.op.Status != OperationReserved || s.payment.Version != original.payment.Version && s.payment.Status != PaymentCaptureFailed {
			return nil
		}
		s.op.DispatchUntil = time.Time{}
		if s.op.AwaitingEvidence && definitiveFailure(cause) == "" {
			cause = ErrNotDispatched
		}
		s.op.AwaitingEvidence = false
		next := OperationUnknown
		if kind := definitiveFailure(cause); kind != "" {
			s.op.Failure = kind
			var providerError *ProviderError
			if errors.As(cause, &providerError) {
				s.op.FailureStatus = sanitizeStatus(providerError.ProviderStatus)
				s.op.RetryAfter = max(0, providerError.RetryAfter)
				if s.op.RetryAfter > 0 {
					s.op.RetryNotBefore = e.config.Now().UTC().Add(s.op.RetryAfter)
				}
			}
			next = OperationFailed
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

// ProcessVerifiedEvent processes the durable, verified inbox row at this
// engine's route and event ID. The normalizer must derive the event identity
// and observation from the stored evidence using only local computation. Its
// receipt, payment, audit and outbox writes commit in the same transaction.
// A delivery replay after commit succeeds without running the normalizer.
func (e *Engine) ProcessVerifiedEvent(ctx context.Context, id string, normalize func(json.RawMessage) (VerifiedEvent, error)) error {
	if normalize == nil {
		return ErrValidation
	}
	return e.ProcessVerifiedEventWithRecords(ctx, id, func(_ EventRecords, raw json.RawMessage) (VerifiedEvent, error) {
		return normalize(raw)
	})
}

// EventRecords exposes the read operations needed to correlate signed provider
// references and turn a modification amount into a cumulative funds snapshot.
type EventRecords interface {
	Payment(Scope, string) (Payment, error)
	Operation(Scope, string) (OperationRecord, error)
	OperationByReference(Scope, string, string) (OperationRecord, error)
	ActiveOperation(Scope, string) (OperationRecord, error)
}

// ProcessVerifiedEventWithRecords runs normalize with the same transactional
// snapshot as receipt processing and result application. Use only local reads
// and computation in normalize.
func (e *Engine) ProcessVerifiedEventWithRecords(ctx context.Context, id string, normalize func(EventRecords, json.RawMessage) (VerifiedEvent, error)) error {
	if strings.TrimSpace(id) == "" || normalize == nil {
		return ErrValidation
	}
	key := VerifiedEventKey{Scope: e.config.Scope, Provider: e.config.ProviderName, MerchantAccount: e.config.MerchantAccount, ID: id}
	return e.config.Store.Transact(ctx, func(tx Tx) error {
		inbox, ok := tx.(VerifiedEventTx)
		if !ok {
			return ErrUnsupported
		}
		return inbox.ProcessVerifiedEvent(key, e.config.Now().UTC(), func(evidence json.RawMessage) error {
			event, err := normalize(tx, evidence)
			if err != nil {
				return err
			}
			if event.ID != id || event.Provider != key.Provider {
				return fmt.Errorf("%w: verified event identity does not match inbox", ErrConflict)
			}
			_, err = e.applyTx(tx, state{}, event.Observation, "webhook")
			return err
		})
	})
}

func (e *Engine) apply(ctx context.Context, original state, o Observation, source string) (state, error) {
	var final state
	err := e.config.Store.Transact(ctx, func(tx Tx) error {
		var err error
		final, err = e.applyTx(tx, original, o, source)
		return err
	})
	return final, err
}

func (e *Engine) applyTx(tx Tx, original state, o Observation, source string) (state, error) {
	id := original.op.ID
	if source == "webhook" {
		if o.Scope != e.config.Scope || o.MerchantAccount != e.config.MerchantAccount {
			return state{}, fmt.Errorf("%w: wrong callback account", ErrConflict)
		}
		id = o.OperationID
		if id == "" {
			rec, err := tx.OperationByReference(o.Scope, o.PaymentID, o.OperationReference)
			if err != nil {
				return state{}, err
			}
			id = rec.ID
		}
	}
	s, err := e.load(tx, id)
	if err != nil {
		return state{}, err
	}
	if err := bind(s, o); err != nil {
		return state{}, err
	}

	failureReference := o.OperationReference
	if o.AutomaticCaptureReference != "" && !o.CaptureFailure {
		return state{}, ErrConflict
	}
	if o.CaptureFailure {
		manual := s.op.Kind == OpCapture && o.OperationReference != "" && o.AutomaticCaptureReference == ""
		automatic := s.payment.Market.Method == MethodACH && (s.op.Kind == OpCreate || s.op.Kind == OpConfirm) &&
			o.AutomaticCaptureReference != "" && o.AutomaticCaptureReference != o.ProviderReference && o.OperationReference == ""
		if automatic {
			failureReference = o.AutomaticCaptureReference
		}
		status := OperationFailed
		if s.op.Status == OperationSucceeded {
			status = OperationSucceeded
		}
		if source != "webhook" || (!manual && !automatic) || o.ProviderReference == "" ||
			o.Snapshot.Status != PaymentCaptureFailed || o.Snapshot.Funds != s.payment.Funds || o.Status != status {
			return state{}, ErrConflict
		}
	} else if o.Snapshot.Status == PaymentCaptureFailed && s.payment.Status != PaymentCaptureFailed {
		return state{}, ErrConflict
	}
	// A mutation reply is fenced by its original reservation. Cumulative
	// settlement/return/dispute evidence may refer to an older operation.
	versionChanged := s.payment.Version != original.payment.Version
	if source == "response" && (versionChanged || s.op != original.op) {
		// A verified acknowledgement can arrive after a hold or a same-key
		// takeover. Retain only the operation PSP and pending status against
		// the current snapshot. A completed callback still wins.
		if !s.acceptsPendingAck(original, o) {
			return s, nil
		}
		o.Snapshot = s.payment.Snapshot
	}
	earlierOperation := s.op.PaymentVersion != s.payment.Version
	if source == "query" && versionChanged && !earlierOperation {
		return s, nil
	}
	if earlierOperation && !o.CaptureFailure && !s.canReconcile(o, source) {
		return state{}, fmt.Errorf("%w: stale observation for an earlier payment version", ErrConflict)
	}
	active, activeErr := tx.ActiveOperation(s.payment.Scope, s.payment.ID)
	if activeErr == nil && active.ID != s.op.ID && !o.CaptureFailure {
		return state{}, fmt.Errorf("%w: another operation is in progress", ErrConflict)
	} else if activeErr != nil && !errors.Is(activeErr, ErrNotFound) {
		return state{}, activeErr
	}
	if o.Snapshot.VerifiedMethod == "" {
		o.Snapshot.VerifiedMethod = s.payment.VerifiedMethod
	}
	if o.Snapshot.VerifiedMethod != "" && o.Snapshot.VerifiedMethod != s.payment.Market.Method {
		return state{}, ErrConflict
	}
	o.Snapshot = o.Snapshot.principalSnapshot(s.payment.Status)
	o.Snapshot.ProviderStatus = sanitizeStatus(o.Snapshot.ProviderStatus)
	o.Snapshot.Action.ExpiresAt = o.Snapshot.Action.ExpiresAt.UTC()
	if err := validateOutcome(s, o, source); err != nil {
		return state{}, err
	}
	at := e.config.Now().UTC()
	if o.CaptureFailure {
		// A second capture can fail after another operation already put this
		// payment on hold. Each failed operation still needs its own host notice.
		review, ok := tx.(CaptureFailureTx)
		if !ok {
			return state{}, ErrUnsupported
		}
		notice := CaptureFailureNotice{
			Scope: s.payment.Scope, Provider: s.payment.Provider, MerchantAccount: s.payment.MerchantAccount,
			PaymentID: s.payment.ID, OrderID: s.payment.OrderID, OperationID: s.op.ID,
			ProviderReference: o.ProviderReference, OperationReference: failureReference, Amount: o.Amount, At: at,
		}
		if err := review.EnqueueCaptureFailure(notice); err != nil {
			return state{}, err
		}
	}
	if s.op.Status == o.Status && s.payment.Snapshot == o.Snapshot &&
		(o.ProviderReference == "" || s.payment.ProviderReference == o.ProviderReference) &&
		(o.OperationReference == "" || s.op.ProviderReference == o.OperationReference) {
		return s, nil
	}

	if err := s.op.SetStatus(o.Status); err != nil {
		return state{}, err
	}
	s.op.DispatchUntil = time.Time{}
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
	change, err := s.payment.Apply(o.Snapshot, at)
	if err != nil {
		return state{}, err
	}
	if !earlierOperation {
		s.op.PaymentVersion = s.payment.Version
	}
	if err := tx.UpdatePayment(s.payment); err != nil {
		return state{}, err
	}
	if err := tx.UpdateOperation(s.op); err != nil {
		return state{}, err
	}
	if err := e.audit(tx, s, change, source); err != nil {
		return state{}, err
	}
	if o.CaptureFailure && activeErr == nil && active.ID != s.op.ID && active.PaymentVersion == change.Version-1 {
		// The hold changes no principal. Keep an unrelated in-flight reservation
		// available to receive its own verified outcome against the held payment.
		active.PaymentVersion = change.Version
		if err := tx.UpdateOperation(active); err != nil {
			return state{}, err
		}
	}
	if o.Status == OperationSucceeded && fullyCollected(s.payment) {
		entry := OutboxEntry{
			Scope: s.payment.Scope, ID: s.payment.ID, PaymentID: s.payment.ID, OrderID: s.payment.OrderID,
			OperationID: s.op.ID, Version: s.payment.Version, At: change.At,
		}
		if err := tx.Enqueue(entry); err != nil {
			return state{}, err
		}
	}
	return s, nil
}

func (s state) acceptsPendingAck(original state, o Observation) bool {
	if s.op.ReplayFingerprint == "" || s.op.Status != OperationReserved && s.op.Status != OperationUnknown ||
		s.op.PaymentVersion != s.payment.Version || o.Status != OperationPending || o.OperationReference == "" ||
		o.ProviderReference == "" || o.ProviderReference != s.payment.ProviderReference || o.Snapshot != original.payment.Snapshot {
		return false
	}
	switch s.op.Kind {
	case OpCapture, OpRefund, OpVoid:
		return s.payment.Snapshot == original.payment.Snapshot || s.payment.Status == PaymentCaptureFailed
	}
	return false
}

// Older reservations may contribute independent cumulative facts. Their
// observations must agree with the current principal and the bound processor
// payment. They cannot authorize, capture, refund or release money again.
func (s state) canReconcile(o Observation, source string) bool {
	if source == "response" || s.op.Status != OperationSucceeded || o.Status != OperationSucceeded ||
		o.ProviderReference == "" || o.ProviderReference != s.payment.ProviderReference {
		return false
	}
	before, after := s.payment.Funds, o.Snapshot.Funds
	if after.Authorized != before.Authorized || after.Captured != before.Captured || after.Refunded != before.Refunded {
		return false
	}
	switch o.Snapshot.Status {
	case PaymentSettled, PaymentReturned:
		return true
	default:
		return o.Snapshot.Status == s.payment.Status &&
			(after.Settled > before.Settled || after.Returned > before.Returned || after.Disputed > before.Disputed ||
				o.Snapshot == s.payment.Snapshot)
	}
}

func fullyCollected(p Payment) bool {
	if p.VerifiedMethod == "" || p.VerifiedMethod != p.Market.Method {
		return false
	}
	f := p.Funds
	if f.Refunded != 0 || f.Returned != 0 || f.Disputed != 0 {
		return false
	}
	if p.Market.Method == MethodACH {
		// A settlement with prior authorization retains its principal status.
		switch p.Status {
		case PaymentSettled, PaymentAuthorized, PaymentPartiallyCaptured, PaymentCaptured:
			return f.Settled == p.Amount.Minor
		}
		return false
	}
	return p.Status == PaymentCaptured && f.Captured == p.Amount.Minor
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
	if p.Market.Method != MethodACH && f.Settled > f.Captured {
		return fmt.Errorf("%w: settlement exceeds captured principal", ErrConflict)
	}
	terminal := op.Status == OperationFailed || op.Status == OperationSucceeded
	// Creating a session may succeed before the provider collects the money.
	// Capture, refund and void reservations each authorize exactly one change.
	session := op.Kind == OpCreate || op.Kind == OpConfirm
	principalChanged := f.Authorized != base.Authorized || f.Captured != base.Captured || f.Refunded != base.Refunded
	if terminal && (op.Status != o.Status || !session && principalChanged) {
		return fmt.Errorf("%w: conflicting terminal observation", ErrConflict)
	}
	if o.Status == OperationFailed && (session && f != (Funds{}) || principalChanged || f.Settled != base.Settled) {
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
		heldVoid := source == "webhook" && p.Status == PaymentCaptureFailed && p.Snapshot == o.Snapshot &&
			o.OperationReference != "" && o.ProviderReference == p.ProviderReference && p.ProviderReference != ""
		if f.Captured != base.Captured || f.Refunded != base.Refunded || f.Authorized != base.Authorized ||
			o.Status == OperationSucceeded && o.Snapshot.Status != PaymentVoided && !heldVoid {
			return fmt.Errorf("%w: void differs from reservation", ErrConflict)
		}
	}
	return nil
}

// Recover asks the adapter for an authoritative result using the persisted
// reservation and original provider key. Recovery never executes a new host
// mutation. A provider may bind an identical-request replay at reservation time.
// Only a read-only query retries on rate limiting/transport errors.
func (e *Engine) Recover(ctx context.Context, operationID string) (Result, error) {
	var original state
	err := e.config.Store.Transact(ctx, func(tx Tx) error {
		var err error
		original, err = e.load(tx, operationID)
		if err == nil && e.caps.QueryAfterDispatch && original.op.Status == OperationReserved && e.config.Now().UTC().Before(original.op.DispatchUntil) {
			return ErrConflict
		}
		if err == nil && original.op.Status == OperationReserved && original.op.AwaitingEvidence && !original.op.DispatchUntil.IsZero() {
			if e.config.Now().Before(original.op.DispatchUntil) {
				return ErrConflict
			}
			// The checkpoint and this takeover are mutually exclusive inside the
			// transaction. An owner that resumes late cannot send the mutation.
			original.op.AwaitingEvidence = false
			original.op.DispatchUntil = time.Time{}
			original.op.Failure = ErrNotDispatched
			if err := original.op.SetStatus(OperationFailed); err != nil {
				return err
			}
			if err := tx.UpdateOperation(original.op); err != nil {
				return err
			}
			return e.audit(tx, original, Change{}, "undispatched_recovery")
		}
		if err == nil && !e.caps.Query {
			return e.claimReplay(tx, &original)
		}
		return err
	})
	if err != nil {
		return Result{original.payment, original.op.Operation}, err
	}
	if original.op.Status == OperationFailed && original.op.Failure == ErrNotDispatched {
		return original.result()
	}
	if !e.caps.Query {
		return e.replayUnknown(ctx, original)
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

// Transfer dispatch ownership under the store's serializable transaction.
// Once a prior lease expires its outstanding HTTP call might still succeed.
// Marking the outcome unknown fences a late definite rejection from that call.
func (e *Engine) claimReplay(tx Tx, s *state) error {
	p, op := s.payment, s.op.Operation
	provider, ok := e.config.Provider.(IdempotentReplayProvider)
	if !ok || op.ReplayFingerprint == "" || op.Status != OperationUnknown && op.Status != OperationReserved || p.Status == PaymentCaptureFailed {
		return ErrUnsupported
	}
	if err := e.checkStore(op.Kind, tx); err != nil {
		return err
	}
	req := ProviderRequest{Payment: p, Operation: op}
	id, window := provider.ReplayIdentity(req)
	if id != op.ReplayFingerprint {
		return ErrConflict
	}
	now := e.config.Now().UTC()
	remaining := op.ReplayUntil.Sub(now)
	if remaining <= 0 || remaining > window {
		return ErrUnsupported
	}
	if s.op.DispatchUntil.After(now) {
		return ErrConflict
	}
	claim := s.op
	claim.Status = OperationUnknown
	claim.DispatchUntil = now.Add(min(e.config.Timeout, remaining))
	if err := tx.UpdateOperation(claim); err != nil {
		return err
	}
	s.op = claim
	return nil
}

// replayUnknown makes one token-free request under the committed lease.
func (e *Engine) replayUnknown(ctx context.Context, original state) (Result, error) {
	req := ProviderRequest{Payment: original.payment, Operation: original.op.Operation}
	remaining := original.op.DispatchUntil.Sub(e.config.Now().UTC())
	call, cancel := context.WithTimeout(ctx, min(e.config.Timeout, remaining))
	defer cancel()
	var obs Observation
	err := call.Err()
	if err == nil {
		obs, err = e.config.Provider.Execute(call, req)
	}
	return e.finishDispatch(ctx, original, obs, err)
}
