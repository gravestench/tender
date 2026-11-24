package tender_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/store"
)

type processor struct {
	execute func(context.Context, tender.ProviderRequest) (tender.Observation, error)
	query   func(context.Context, tender.ProviderRequest) (tender.Observation, error)
	calls   atomic.Int32
}

var usVisa = tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}
var usd100 = tender.Money{Currency: "USD", Minor: 100}
var shop = tender.Scope{MerchantID: "shop", Environment: tender.EnvironmentMock}

func (*processor) Capabilities() tender.Capabilities {
	return tender.Capabilities{Create: true, Confirm: true, Capture: true, Void: true, Refund: true, Query: true, Markets: []tender.MarketCapability{{Market: usVisa, PartialCapture: true, PartialRefund: true}}}
}
func (p *processor) Execute(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
	p.calls.Add(1)
	return p.execute(ctx, r)
}
func (p *processor) Query(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
	return p.query(ctx, r)
}
func observed(r tender.ProviderRequest, status tender.PaymentStatus, funds tender.Funds) tender.Observation {
	return tender.Observation{Scope: r.Payment.Scope, MerchantAccount: r.Payment.MerchantAccount, PaymentID: r.Payment.ID, AttemptID: r.Operation.AttemptID, OperationID: r.Operation.ID, ProviderReference: "remote-" + r.Payment.ID, OperationReference: "remote-" + r.Operation.ID, Amount: r.Operation.Amount, Status: tender.OperationSucceeded, Snapshot: tender.Snapshot{Status: status, Funds: funds}}
}
func authorized(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
	return observed(r, tender.PaymentAuthorized, tender.Funds{Authorized: 100}), nil
}
func openEngine(t *testing.T, db tender.Store, p tender.Provider, scope tender.Scope) *tender.Engine {
	t.Helper()
	e, err := tender.NewEngine(tender.EngineConfig{Store: db, Provider: p, Scope: scope, ProviderName: "acquirer", MerchantAccount: "merchant-account", Market: usVisa})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func purchase() tender.Request {
	return tender.Request{PaymentID: "payment", OrderID: "order", Key: "create", Amount: usd100, Token: "opaque"}
}
func getRequest(t *testing.T, requests chan tender.ProviderRequest) tender.ProviderRequest {
	t.Helper()
	select {
	case req := <-requests:
		return req
	case <-time.After(3 * time.Second):
		t.Fatal("provider was never called")
		return tender.ProviderRequest{}
	}
}

type response struct {
	result tender.Result
	err    error
}

func TestConcurrentIdempotency(t *testing.T) {
	db := store.NewMemory()
	started, release := make(chan tender.ProviderRequest, 1), make(chan struct{})
	defer close(release)
	p := &processor{execute: func(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		started <- r
		select {
		case <-release:
		case <-ctx.Done():
			return tender.Observation{}, ctx.Err()
		}
		return authorized(ctx, r)
	}}
	engine := openEngine(t, db, p, shop)
	done := make(chan response, 1)
	go func() { r, err := engine.Create(t.Context(), purchase()); done <- response{r, err} }()
	req := getRequest(t, started)
	if req.Payment.Market != usVisa {
		t.Fatalf("adapter market = %+v", req.Payment.Market)
	}
	// Re-enter from another engine while the provider is still blocked.
	other := openEngine(t, db, p, shop)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	r, err := other.Create(ctx, purchase())
	if err != nil || r.Operation.ID != req.Operation.ID || r.Operation.Status != tender.OperationReserved {
		t.Fatalf("duplicate reservation = %+v, %v", r, err)
	}
	for _, edit := range []func(*tender.Request){func(r *tender.Request) { r.Amount.Minor++ }, func(r *tender.Request) { r.Token = "different" }, func(r *tender.Request) { r.OrderID = "other" }, func(r *tender.Request) { r.PaymentID = "other" }} {
		changed := purchase()
		edit(&changed)
		if _, err = other.Create(ctx, changed); !errors.Is(err, tender.ErrConflict) {
			t.Fatalf("changed request accepted: %v", err)
		}
	}
	release <- struct{}{}
	first := <-done
	replay, err := other.Create(ctx, purchase())
	if first.err != nil || err != nil || first.result != replay || replay.Payment.Status != tender.PaymentAuthorized || p.calls.Load() != 1 {
		t.Fatalf("completed replay = %+v; first = %+v; calls %d; err %v", replay, first, p.calls.Load(), err)
	}
}

func TestConcurrentOrderClaim(t *testing.T) {
	db := store.NewMemory()
	p := &processor{execute: authorized}
	const n = 16
	out := make(chan response, n)
	for i := range n {
		e := openEngine(t, db, p, shop)
		go func() {
			r := purchase()
			r.Key += string(rune('a' + i))
			r.PaymentID += string(rune('a' + i))
			res, err := e.Create(t.Context(), r)
			out <- response{res, err}
		}()
	}
	success := 0
	for range n {
		r := <-out
		if r.err == nil {
			success++
		} else if !errors.Is(r.err, tender.ErrConflict) {
			t.Fatal(r.err)
		}
	}
	if success != 1 || p.calls.Load() != 1 {
		t.Fatalf("same order got %d successes and %d provider calls", success, p.calls.Load())
	}
	for _, scope := range []tender.Scope{{MerchantID: "other-shop", Environment: tender.EnvironmentMock}, {MerchantID: "shop", Environment: tender.EnvironmentSandbox}} {
		e := openEngine(t, db, p, scope)
		if _, err := e.Create(t.Context(), purchase()); err != nil {
			t.Fatalf("independent scope %+v: %v", scope, err)
		}
	}
	if p.calls.Load() != 3 {
		t.Fatal("scoped order reused an unrelated charge")
	}
}

func TestConflictingAmounts(t *testing.T) {
	db := store.NewMemory()
	started, release := make(chan tender.ProviderRequest, 1), make(chan struct{}, 1)
	p := &processor{execute: func(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		if r.Operation.Kind == tender.OpCreate {
			return authorized(ctx, r)
		}
		started <- r
		select {
		case <-release:
		case <-ctx.Done():
			return tender.Observation{}, ctx.Err()
		}
		funds := r.Payment.Funds
		if r.Operation.Kind == tender.OpCapture {
			funds.Captured += r.Operation.Amount.Minor
		} else {
			funds.Refunded += r.Operation.Amount.Minor
		}
		return observed(r, tender.PaymentPartiallyCaptured, funds), nil
	}}
	e := openEngine(t, db, p, shop)
	if _, err := e.Create(t.Context(), purchase()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind           string
		fn             func(context.Context, tender.Request) (tender.Result, error)
		amount, excess int64
	}{{"capture", e.Capture, 70, 31}, {"refund", e.Refund, 50, 21}} {
		r := tender.Request{PaymentID: "payment", Key: tc.kind, Amount: tender.Money{Minor: tc.amount, Currency: "USD"}}
		done := make(chan error, 1)
		go func() { _, err := tc.fn(t.Context(), r); done <- err }()
		getRequest(t, started)
		competing := r
		competing.Key += "-more"
		competing.Amount.Minor = tc.excess
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		_, err := tc.fn(ctx, competing)
		cancel()
		if !errors.Is(err, tender.ErrConflict) {
			t.Fatalf("%s concurrent excess: %v", tc.kind, err)
		}
		release <- struct{}{}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if _, err := tc.fn(t.Context(), competing); !errors.Is(err, tender.ErrValidation) {
			t.Fatalf("%s cumulative excess: %v", tc.kind, err)
		}
	}
	payment, err := e.Get(t.Context(), "payment")
	if err != nil || payment.Funds != (tender.Funds{Authorized: 100, Captured: 70, Refunded: 50}) || p.calls.Load() != 3 {
		t.Fatalf("payment=%+v calls=%d err=%v", payment, p.calls.Load(), err)
	}
}

func TestUnknownOutcomeRecovery(t *testing.T) {
	db := store.NewMemory()
	var committed tender.ProviderRequest
	p := &processor{execute: func(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		committed = r
		return tender.Observation{}, context.DeadlineExceeded
	}}
	var queries atomic.Int32
	p.query = func(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("unbounded provider query")
		}
		if r.Operation.ProviderKey == "" || r.Operation.ProviderKey != committed.Operation.ProviderKey || r.Operation.ID != committed.Operation.ID || r.Token != "" {
			t.Fatalf("query lost reservation or included transient input")
		}
		if queries.Add(1) == 1 {
			return tender.Observation{}, &tender.ProviderError{Kind: tender.ErrRateLimit, RetryAfter: 15 * time.Millisecond}
		}
		return observed(r, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100}), nil
	}
	first, err := openEngine(t, db, p, shop).Create(t.Context(), purchase())
	if !errors.Is(err, tender.ErrUnknownOutcome) || first.Operation.Status != tender.OperationUnknown || first.Payment.Status != tender.PaymentCreated {
		t.Fatalf("lost response: %+v %v", first, err)
	}
	restarted := openEngine(t, db, p, shop)
	if _, err := restarted.Create(t.Context(), purchase()); !errors.Is(err, tender.ErrUnknownOutcome) {
		t.Fatalf("unknown replay: %v", err)
	}
	if _, err := restarted.Void(t.Context(), tender.Request{PaymentID: "payment", Key: "void", Amount: usd100}); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("unknown allowed conflict: %v", err)
	}
	at := time.Now()
	fixed, err := restarted.Recover(t.Context(), first.Operation.ID)
	if err != nil || fixed.Operation.Status != tender.OperationSucceeded || fixed.Payment.Status != tender.PaymentCaptured || time.Since(at) < 15*time.Millisecond {
		t.Fatalf("recovered: %+v %v", fixed, err)
	}
	if replay, err := restarted.Create(t.Context(), purchase()); err != nil || replay != fixed || p.calls.Load() != 1 {
		t.Fatalf("replayed mutation: %+v %v calls=%d", replay, err, p.calls.Load())
	}
	out, err := db.Outbox(t.Context(), shop)
	if err != nil || len(out) != 1 || out[0].PaymentID != "payment" || out[0].Version != fixed.Payment.Version {
		t.Fatalf("fulfillment: %+v %v", out, err)
	}
	history, err := db.History(t.Context(), shop, "payment")
	if err != nil || len(history) != 3 || history[0].OperationStatus != tender.OperationReserved || history[1].OperationStatus != tender.OperationUnknown || history[2].Change.After.Status != tender.PaymentCaptured {
		t.Fatalf("history: %+v %v", history, err)
	}
}

func TestVerifiedObservationPrecedence(t *testing.T) {
	db := store.NewMemory()
	started, release := make(chan tender.ProviderRequest, 1), make(chan struct{})
	defer close(release)
	p := &processor{execute: func(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		started <- r
		select {
		case <-release:
		case <-ctx.Done():
		}
		return tender.Observation{}, tender.ErrTransport
	}}
	e := openEngine(t, db, p, shop)
	done := make(chan response, 1)
	go func() { r, err := e.Create(t.Context(), purchase()); done <- response{r, err} }()
	req := getRequest(t, started)
	obs := observed(req, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100})
	for _, edit := range []func(*tender.Observation){func(o *tender.Observation) { o.Scope.MerchantID = "other" }, func(o *tender.Observation) { o.MerchantAccount = "other" }, func(o *tender.Observation) { o.PaymentID = "other" }, func(o *tender.Observation) { o.AttemptID = "other" }, func(o *tender.Observation) { o.OperationID = "other" }, func(o *tender.Observation) { o.Amount.Minor-- }, func(o *tender.Observation) { o.Amount.Currency = "EUR" }} {
		bad := obs
		edit(&bad)
		if _, err := e.ApplyVerified(t.Context(), bad); err == nil {
			t.Fatal("accepted misbound observation")
		}
	}
	good, err := e.ApplyVerified(t.Context(), obs)
	if err != nil {
		t.Fatal(err)
	}
	replay := obs
	replay.OperationID = ""
	replay.AttemptID = ""
	dup, err := e.ApplyVerified(t.Context(), replay)
	if err != nil || dup != good {
		t.Fatalf("replay via provider operation reference: %+v %v", dup, err)
	}
	for _, edit := range []func(*tender.Observation){func(o *tender.Observation) { o.ProviderReference = "other" }, func(o *tender.Observation) { o.OperationReference = "other" }} {
		bad := obs
		edit(&bad)
		if _, err := e.ApplyVerified(t.Context(), bad); err == nil {
			t.Fatal("accepted changed provider reference")
		}
	}
	release <- struct{}{}
	late := <-done
	out, err := db.Outbox(t.Context(), shop)
	if late.err != nil || late.result != good || err != nil || len(out) != 1 {
		t.Fatalf("late timeout changed settled result: %+v out=%+v err=%v", late, out, err)
	}
}

func TestAuthorityDuringRecovery(t *testing.T) {
	db := store.NewMemory()
	executing, querying := make(chan tender.ProviderRequest, 1), make(chan tender.ProviderRequest, 1)
	endExecute, endQuery := make(chan struct{}), make(chan struct{})
	p := &processor{
		execute: func(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
			executing <- r
			select {
			case <-endExecute:
			case <-ctx.Done():
			}
			return tender.Observation{}, errors.Join(tender.ErrUnknownOutcome, tender.ErrRateLimit)
		},
		query: func(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
			querying <- r
			select {
			case <-endQuery:
			case <-ctx.Done():
			}
			return authorized(ctx, r)
		},
	}
	e := openEngine(t, db, p, shop)
	created, queried := make(chan response, 1), make(chan response, 1)
	go func() { r, err := e.Create(t.Context(), purchase()); created <- response{r, err} }()
	req := getRequest(t, executing)
	go func() { r, err := e.Recover(t.Context(), req.Operation.ID); queried <- response{r, err} }()
	getRequest(t, querying)
	close(endExecute)
	first := <-created
	close(endQuery)
	recovery := <-queried
	if first.result.Operation.Status != tender.OperationUnknown || !errors.Is(first.err, tender.ErrUnknownOutcome) {
		t.Errorf("ambiguous dispatch resolved definitively: %+v", first)
	}
	if recovery.err != nil || recovery.result.Payment.Status != tender.PaymentAuthorized || recovery.result.Operation.Status != tender.OperationSucceeded {
		t.Fatalf("lost authoritative query: %+v", recovery)
	}
}

func TestVerifiedPaymentLifecycle(t *testing.T) {
	db := store.NewMemory()
	var req tender.ProviderRequest
	p := &processor{execute: func(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		req = r
		return observed(r, tender.PaymentPending, tender.Funds{}), nil
	}}
	e := openEngine(t, db, p, shop)
	if _, err := e.Create(t.Context(), purchase()); err != nil {
		t.Fatal(err)
	}
	// Creating a provider session can succeed before money is collected.
	collect := observed(req, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100})
	captured, err := e.ApplyVerified(t.Context(), collect)
	if err != nil || captured.Payment.Status != tender.PaymentCaptured {
		t.Fatalf("session completion: %+v %v", captured, err)
	}
	settle := observed(req, tender.PaymentSettled, tender.Funds{Authorized: 100, Captured: 100, Settled: 100})
	got, err := e.ApplyVerified(t.Context(), settle)
	if err != nil {
		t.Fatal(err)
	}
	dup, err := e.ApplyVerified(t.Context(), settle)
	if err != nil || dup != got {
		t.Fatalf("settlement replay changed version: %+v, %+v, %v", got, dup, err)
	}
	out, err := db.Outbox(t.Context(), shop)
	if err != nil || len(out) != 1 {
		t.Fatalf("fulfillment count: %d, %v", len(out), err)
	}
}

func TestInvalidProcessorResultLeavesRecoveryOpen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status tender.PaymentStatus
		op     tender.OperationStatus
		funds  tender.Funds
	}{
		{"failed charge with collection", tender.PaymentCaptured, tender.OperationFailed, tender.Funds{Authorized: 100, Captured: 100}},
		{"declined charge marked successful", tender.PaymentDeclined, tender.OperationSucceeded, tender.Funds{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := store.NewMemory()
			p := &processor{execute: func(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
				o := observed(r, tc.status, tc.funds)
				o.Status = tc.op
				return o, nil
			}}
			r, err := openEngine(t, db, p, shop).Create(t.Context(), purchase())
			if !errors.Is(err, tender.ErrUnknownOutcome) || r.Operation.Status != tender.OperationUnknown || r.Payment.Status != tender.PaymentCreated {
				t.Fatalf("inconsistent charge escaped recovery: %+v %v", r, err)
			}
			entries, err := db.Outbox(t.Context(), shop)
			if err != nil || len(entries) != 0 {
				t.Fatalf("unverified fulfillment: %+v %v", entries, err)
			}
		})
	}
}

func TestVerifiedDeclineAcknowledgement(t *testing.T) {
	db := store.NewMemory()
	var req tender.ProviderRequest
	p := &processor{execute: func(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		req = r
		return tender.Observation{}, tender.ErrUnknownOutcome
	}}
	e := openEngine(t, db, p, shop)
	_, _ = e.Create(t.Context(), purchase())
	declined := observed(req, tender.PaymentDeclined, tender.Funds{})
	declined.Status = tender.OperationFailed
	applied, err := e.ApplyVerified(t.Context(), declined)
	if err != nil || applied.Payment.Status != tender.PaymentDeclined {
		t.Fatalf("verified decline not acknowledged: %+v %v", applied, err)
	}
	r, err := e.Create(t.Context(), purchase())
	if !errors.Is(err, tender.ErrDeclined) || r != applied || p.calls.Load() != 1 {
		t.Fatalf("decline replay = %+v %v", r, err)
	}
}

func TestDefinitiveErrorReplay(t *testing.T) {
	db := store.NewMemory()
	p := &processor{execute: func(context.Context, tender.ProviderRequest) (tender.Observation, error) {
		return tender.Observation{}, &tender.ProviderError{Kind: tender.ErrRateLimit, ProviderStatus: "Throttled", RetryAfter: 2 * time.Second}
	}}
	e := openEngine(t, db, p, shop)
	for range 2 {
		r, err := e.Create(t.Context(), purchase())
		var public *tender.ProviderError
		if r.Operation.Status != tender.OperationFailed || !errors.As(err, &public) || public.Kind != tender.ErrRateLimit || public.ProviderStatus != "Throttled" || public.RetryAfter != 2*time.Second {
			t.Fatalf("missing public retry metadata: %v %+v", err, public)
		}
	}
	if p.calls.Load() != 1 {
		t.Fatalf("retried mutation %d times", p.calls.Load())
	}
}

func TestResultCommitFailure(t *testing.T) {
	db := store.NewMemory()
	fault := &failOutboxStore{Store: db}
	var req tender.ProviderRequest
	p := &processor{execute: func(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		req = r
		return observed(r, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100}), nil
	}, query: func(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		return observed(r, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100}), nil
	}}
	e := openEngine(t, fault, p, shop)
	r, err := e.Create(t.Context(), purchase())
	if !errors.Is(err, tender.ErrUnknownOutcome) || r.Operation.Status != tender.OperationUnknown || r.Payment.Version != 1 {
		t.Fatalf("failed commit escaped: %+v %v", r, err)
	}
	h, err := db.History(t.Context(), shop, "payment")
	if err != nil || len(h) != 2 || h[1].OperationStatus != tender.OperationUnknown {
		t.Fatalf("leaked payment history %+v %v", h, err)
	}
	if out, err := db.Outbox(t.Context(), shop); err != nil || len(out) != 0 {
		t.Fatalf("leaked outbox: %+v %v", out, err)
	}
	restarted := openEngine(t, db, p, shop)
	final, err := restarted.Recover(t.Context(), req.Operation.ID)
	if err != nil || final.Payment.Version != 2 || final.Payment.Funds.Captured != 100 || p.calls.Load() != 1 {
		t.Fatalf("post-commit recovery: %+v %v", final, err)
	}
}

type failOutboxStore struct{ tender.Store }

func (s *failOutboxStore) Transact(ctx context.Context, fn func(tender.Tx) error) error {
	return s.Store.Transact(ctx, func(tx tender.Tx) error { return fn(failOutboxTx{tx}) })
}

type failOutboxTx struct{ tender.Tx }

func (failOutboxTx) Enqueue(tender.OutboxEntry) error { return errors.New("injected outbox failure") }

func TestImmutableProviderRoute(t *testing.T) {
	db := store.NewMemory()
	p := &processor{execute: authorized}
	first, err := openEngine(t, db, p, shop).Create(t.Context(), purchase())
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*tender.EngineConfig){func(c *tender.EngineConfig) { c.ProviderName = "other" }, func(c *tender.EngineConfig) { c.MerchantAccount = "other" }, func(c *tender.EngineConfig) { c.Market.Method = tender.MethodMastercard }} {
		cfg := tender.EngineConfig{Store: db, Provider: p, Scope: shop, ProviderName: "acquirer", MerchantAccount: "merchant-account", Market: usVisa}
		edit(&cfg)
		e, err := tender.NewEngine(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Get(t.Context(), "payment"); !errors.Is(err, tender.ErrConflict) {
			t.Fatalf("read reconfigured route: %v", err)
		}
		if _, err := e.Capture(t.Context(), tender.Request{PaymentID: "payment", Key: "capture", Amount: usd100}); !errors.Is(err, tender.ErrConflict) {
			t.Fatalf("dispatched to reconfigured route: %v", err)
		}
		if _, err := e.Recover(t.Context(), first.Operation.ID); !errors.Is(err, tender.ErrConflict) {
			t.Fatalf("queried reconfigured route: %v", err)
		}
	}
}

func TestNoFulfillmentUntilConfirmed(t *testing.T) {
	db := store.NewMemory()
	var req tender.ProviderRequest
	p := &processor{execute: func(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		req = r
		o := observed(r, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100})
		o.Status = tender.OperationPending
		return o, nil
	}}
	e := openEngine(t, db, p, shop)
	first, err := e.Create(t.Context(), purchase())
	if err != nil || first.Operation.Status != tender.OperationPending {
		t.Fatalf("pending: %+v %v", first, err)
	}
	if out, err := db.Outbox(t.Context(), shop); err != nil || len(out) != 0 {
		t.Fatalf("premature fulfillment: %+v %v", out, err)
	}
	if _, err := e.ApplyVerified(t.Context(), observed(req, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100})); err != nil {
		t.Fatal(err)
	}
	if out, err := db.Outbox(t.Context(), shop); err != nil || len(out) != 1 {
		t.Fatalf("missing fulfillment: %+v %v", out, err)
	}
}

func TestUnorderedActionNeedsQuery(t *testing.T) {
	db := store.NewMemory()
	var req tender.ProviderRequest
	exp := time.Now().UTC().Add(time.Hour)
	ready := func(r tender.ProviderRequest) tender.Observation {
		o := observed(r, tender.PaymentRequiresAction, tender.Funds{})
		o.Snapshot.Action = tender.CustomerAction{Mode: tender.ActionComponent, Data: "current", ExpiresAt: exp}
		return o
	}
	p := &processor{execute: func(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		req = r
		return ready(r), nil
	}, query: func(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) { return ready(r), nil }}
	e := openEngine(t, db, p, shop)
	current, err := e.Create(t.Context(), purchase())
	if err != nil {
		t.Fatal(err)
	}
	old := ready(req)
	old.Snapshot.Action.Data = "stale"
	if _, err := e.ApplyVerified(t.Context(), old); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("unsequenced action overwrote current one: %v", err)
	}
	if got, err := e.Recover(t.Context(), req.Operation.ID); err != nil || got != current {
		t.Fatalf("authoritative query: %+v %v", got, err)
	}
}

func TestVoidPendingSession(t *testing.T) {
	db := store.NewMemory()
	p := &processor{execute: func(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		if r.Operation.Kind == tender.OpCreate {
			return observed(r, tender.PaymentPending, tender.Funds{}), nil
		}
		return observed(r, tender.PaymentVoided, tender.Funds{}), nil
	}}
	e := openEngine(t, db, p, shop)
	if _, err := e.Create(t.Context(), purchase()); err != nil {
		t.Fatal(err)
	}
	r, err := e.Void(t.Context(), tender.Request{PaymentID: "payment", Key: "cancel", Amount: usd100})
	if err != nil || r.Payment.Status != tender.PaymentVoided || p.calls.Load() != 2 {
		t.Fatalf("cancel pending session: %+v %v", r, err)
	}
}

func TestOperationAmountBinding(t *testing.T) {
	db := store.NewMemory()
	p := &processor{execute: func(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		if r.Operation.Kind == tender.OpCreate {
			return authorized(ctx, r)
		}
		return observed(r, tender.PaymentPartiallyCaptured, tender.Funds{Authorized: 100, Captured: 80}), nil
	}}
	e := openEngine(t, db, p, shop)
	if _, err := e.Create(t.Context(), purchase()); err != nil {
		t.Fatal(err)
	}
	r, err := e.Capture(t.Context(), tender.Request{PaymentID: "payment", Key: "capture", Amount: tender.Money{Minor: 70, Currency: "USD"}})
	if !errors.Is(err, tender.ErrUnknownOutcome) || !errors.Is(err, tender.ErrConflict) || r.Payment.Funds.Captured != 0 || r.Operation.Status != tender.OperationUnknown {
		t.Fatalf("accepted more principal than reserved: %+v %v", r, err)
	}
}

func TestEarlierOperationSettlementAndReturn(t *testing.T) {
	for _, query := range []bool{false, true} {
		name := "verified"
		if query {
			name = "query"
		}
		t.Run(name, func(t *testing.T) {
			db := store.NewMemory()
			var create tender.ProviderRequest
			funds := tender.Funds{Authorized: 100, Captured: 100, Settled: 100}
			status := tender.PaymentSettled
			p := &processor{execute: func(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
				if r.Operation.Kind == tender.OpCreate {
					create = r
					return authorized(ctx, r)
				}
				return observed(r, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100}), nil
			}, query: func(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
				if r.Operation.ID != create.Operation.ID {
					t.Fatal("queried different operation")
				}
				return observed(r, status, funds), nil
			}}
			e := openEngine(t, db, p, shop)
			if _, err := e.Create(t.Context(), purchase()); err != nil {
				t.Fatal(err)
			}
			captured, err := e.Capture(t.Context(), tender.Request{PaymentID: "payment", Key: "capture", Amount: usd100})
			if err != nil {
				t.Fatal(err)
			}
			observe := func() (tender.Result, error) {
				if query {
					return e.Recover(t.Context(), create.Operation.ID)
				}
				return e.ApplyVerified(t.Context(), observed(create, status, funds))
			}
			settled, err := observe()
			if err != nil || settled.Payment.Version != captured.Payment.Version+1 || settled.Payment.Funds != funds {
				t.Fatalf("old reservation could not record settlement: %v, %+v", err, settled.Payment)
			}
			duplicate, err := observe()
			if err != nil || duplicate != settled {
				t.Fatalf("duplicate settlement changed history: %v", err)
			}
			funds.Refunded = 40
			if _, err := observe(); err == nil {
				t.Fatal("old operation changed refund principal")
			}
			funds.Refunded = 0
			funds.Returned = 100
			status = tender.PaymentReturned
			returned, err := observe()
			if err != nil || returned.Payment.Status != tender.PaymentReturned || returned.Payment.Funds != funds {
				t.Fatalf("old reservation could not record return: %v, %+v", err, returned.Payment)
			}
			out, err := db.Outbox(t.Context(), shop)
			if err != nil || len(out) != 1 {
				t.Fatalf("changed fulfillment count: %v %v", out, err)
			}
		})
	}
}

func TestRetryDefinitiveCreate(t *testing.T) {
	db := store.NewMemory()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	started, release := make(chan tender.ProviderRequest, 1), make(chan struct{})
	defer close(release)
	p := &processor{execute: func(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		if r.Operation.Key == "create" {
			return tender.Observation{}, &tender.ProviderError{Kind: tender.ErrRateLimit, RetryAfter: time.Minute}
		}
		started <- r
		select {
		case <-release:
		case <-ctx.Done():
		}
		return authorized(ctx, r)
	}}
	cfg := tender.EngineConfig{Store: db, Provider: p, Scope: shop, ProviderName: "acquirer", MerchantAccount: "merchant-account", Market: usVisa, Now: func() time.Time { return now }}
	e, err := tender.NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, err := e.Create(t.Context(), purchase())
	if !errors.Is(err, tender.ErrRateLimit) {
		t.Fatal(err)
	}
	restarted, err := tender.NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second := purchase()
	second.Key = "retry"
	now = now.Add(40 * time.Second)
	_, err = restarted.RetryCreate(t.Context(), first.Operation.ID, second)
	var rate *tender.ProviderError
	if !errors.As(err, &rate) || rate.Kind != tender.ErrRateLimit || rate.RetryAfter != 20*time.Second || p.calls.Load() != 1 {
		t.Fatalf("ignored remaining Retry-After: %v, calls %d", err, p.calls.Load())
	}
	now = now.Add(20 * time.Second)
	wrong := second
	wrong.PaymentID = "other"
	if _, err := restarted.RetryCreate(t.Context(), first.Operation.ID, wrong); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("retry changed payment: %v", err)
	}
	wrong = second
	wrong.Amount.Minor = 99
	if _, err := restarted.RetryCreate(t.Context(), first.Operation.ID, wrong); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("retry changed trusted total: %v", err)
	}
	done := make(chan response, 1)
	go func() {
		r, err := restarted.RetryCreate(t.Context(), first.Operation.ID, second)
		done <- response{r, err}
	}()
	req := getRequest(t, started)
	competitor := second
	competitor.Key = "independent"
	if _, err := restarted.RetryCreate(t.Context(), first.Operation.ID, competitor); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("duplicate successor: %v", err)
	}
	release <- struct{}{}
	success := <-done
	replay, err := restarted.RetryCreate(t.Context(), first.Operation.ID, second)
	if err != nil || success.err != nil || success.result != replay || replay.Payment.Status != tender.PaymentAuthorized || req.Operation.ID == first.Operation.ID || replay.Payment.ID != first.Payment.ID || p.calls.Load() != 2 {
		t.Fatalf("retry did not preserve the claim/idempotency: %+v, %+v, %v", success, replay, err)
	}
	if _, err := restarted.RetryCreate(t.Context(), first.Operation.ID, competitor); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("reused rejected predecessor: %v", err)
	}
	unknownProc := &processor{execute: func(context.Context, tender.ProviderRequest) (tender.Observation, error) {
		return tender.Observation{}, tender.ErrUnknownOutcome
	}}
	unknownEngine := openEngine(t, db, unknownProc, shop)
	diff := purchase()
	diff.OrderID = "other-order"
	diff.PaymentID = "other-payment"
	diff.Key = "unknown"
	unknown, err := unknownEngine.Create(t.Context(), diff)
	if !errors.Is(err, tender.ErrUnknownOutcome) {
		t.Fatal(err)
	}
	diff.Key = "new-key"
	if _, err := restarted.RetryCreate(t.Context(), unknown.Operation.ID, diff); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("retried ambiguous dispatch: %v", err)
	}
}

type achProcessor struct{ *processor }

func (p achProcessor) Capabilities() tender.Capabilities {
	caps := p.processor.Capabilities()
	caps.Markets = []tender.MarketCapability{{Market: tender.Market{Country: "US", Currency: "USD", Method: tender.MethodACH}}}
	return caps
}

func TestFulfillmentCollectionEvidence(t *testing.T) {
	t.Run("voided card cannot settle uncaptured principal", func(t *testing.T) {
		db := store.NewMemory()
		p := &processor{execute: func(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
			if r.Operation.Kind == tender.OpCreate {
				return authorized(ctx, r)
			}
			return observed(r, tender.PaymentVoided, tender.Funds{Authorized: 100, Settled: 100}), nil
		}}
		e := openEngine(t, db, p, shop)
		if _, err := e.Create(t.Context(), purchase()); err != nil {
			t.Fatal(err)
		}
		result, err := e.Void(t.Context(), tender.Request{PaymentID: "payment", Key: "void", Amount: usd100})
		if !errors.Is(err, tender.ErrUnknownOutcome) || result.Payment.Funds.Settled != 0 {
			t.Fatalf("accepted card settlement beyond capture: %v, %+v", err, result.Payment)
		}
		out, err := db.Outbox(t.Context(), shop)
		if err != nil || len(out) != 0 {
			t.Fatalf("fulfilled voided card: %v, %v", out, err)
		}
	})
	t.Run("ACH settlement collects without card principal", func(t *testing.T) {
		db := store.NewMemory()
		p := achProcessor{&processor{execute: func(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
			return observed(r, tender.PaymentSettled, tender.Funds{Settled: 100}), nil
		}}}
		e, err := tender.NewEngine(tender.EngineConfig{Store: db, Provider: p, Scope: shop, ProviderName: "bank", MerchantAccount: "account", Market: p.Capabilities().Markets[0].Market})
		if err != nil {
			t.Fatal(err)
		}
		r, err := e.Create(t.Context(), purchase())
		if err != nil || r.Payment.Status != tender.PaymentSettled {
			t.Fatal(err)
		}
		out, err := db.Outbox(t.Context(), shop)
		if err != nil || len(out) != 1 {
			t.Fatalf("missing ACH collection: %+v %v", out, err)
		}
	})
}

func TestPendingCollectionRejectsContradictoryFailure(t *testing.T) {
	db := store.NewMemory()
	var req tender.ProviderRequest
	p := &processor{execute: func(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		req = r
		o := observed(r, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100})
		o.Status = tender.OperationPending
		return o, nil
	}}
	e := openEngine(t, db, p, shop)
	first, err := e.Create(t.Context(), purchase())
	if err != nil {
		t.Fatal(err)
	}
	failed := observed(req, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100})
	failed.Status = tender.OperationFailed
	if _, err := e.ApplyVerified(t.Context(), failed); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("accepted contradictory failure with provisional captured funds: %v", err)
	}
	r, err := e.Create(t.Context(), purchase())
	if err != nil || r != first {
		t.Fatalf("failed observation changed pending reservation: %+v %v", r, err)
	}
	if _, err := e.Refund(t.Context(), tender.Request{PaymentID: "payment", Key: "refund", Amount: usd100}); !errors.Is(err, tender.ErrConflict) || p.calls.Load() != 1 {
		t.Fatalf("pending funds became refundable: %v, calls %d", err, p.calls.Load())
	}
}

func TestACHRequiresSettlementForFulfillment(t *testing.T) {
	db := store.NewMemory()
	var req tender.ProviderRequest
	funds := tender.Funds{Authorized: 100, Captured: 100}
	p := achProcessor{&processor{execute: func(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		req = r
		return observed(r, tender.PaymentCaptured, funds), nil
	}}}
	e, err := tender.NewEngine(tender.EngineConfig{
		Store: db, Provider: p, Scope: shop, ProviderName: "bank", MerchantAccount: "account",
		Market: p.Capabilities().Markets[0].Market,
	})
	if err != nil {
		t.Fatal(err)
	}

	created, err := e.Create(t.Context(), purchase())
	if err != nil || created.Payment.Funds != funds {
		t.Fatalf("initial ACH principal: %+v %v", created, err)
	}
	out, err := db.Outbox(t.Context(), shop)
	if err != nil || len(out) != 0 {
		t.Fatalf("bank collection published before settlement: %v %v", out, err)
	}

	funds.Settled = 100
	settlement := observed(req, tender.PaymentSettled, funds)
	settled, err := e.ApplyVerified(t.Context(), settlement)
	if err != nil || settled.Payment.Funds != funds {
		t.Fatalf("verified full settlement: %+v %v", settled, err)
	}
	out, err = db.Outbox(t.Context(), shop)
	if err != nil || len(out) != 1 || out[0].Version != settled.Payment.Version {
		t.Fatalf("full ACH settlement failed to publish one collection: %+v %v", out, err)
	}
	notice := out[0]

	replay, err := e.ApplyVerified(t.Context(), settlement)
	if err != nil || replay != settled {
		t.Fatalf("settlement replay changed payment: %+v %v", replay, err)
	}
	out, err = db.Outbox(t.Context(), shop)
	if err != nil || len(out) != 1 || out[0] != notice {
		t.Fatalf("settlement replay changed outbox: %+v %v", out, err)
	}

	funds.Returned = 100
	returned, err := e.ApplyVerified(t.Context(), observed(req, tender.PaymentReturned, funds))
	if err != nil || returned.Payment.Status != tender.PaymentReturned || returned.Payment.Funds != funds {
		t.Fatalf("return lost original settlement or failed to reverse collection: %+v %v", returned, err)
	}
	out, err = db.Outbox(t.Context(), shop)
	if err != nil || len(out) != 1 || out[0] != notice {
		t.Fatalf("return changed historical collection notice: %+v %v", out, err)
	}
	if _, err := e.ApplyVerified(t.Context(), settlement); err == nil {
		t.Fatal("late settlement reversed the recorded return")
	}
}

func TestRetryCreateRejectsAmbiguousTransport(t *testing.T) {
	db := store.NewMemory()
	p := &processor{execute: func(context.Context, tender.ProviderRequest) (tender.Observation, error) {
		return tender.Observation{}, errors.Join(tender.ErrRateLimit, tender.ErrTransport)
	}}
	e := openEngine(t, db, p, shop)
	r, err := e.Create(t.Context(), purchase())
	if !errors.Is(err, tender.ErrUnknownOutcome) || r.Operation.Status != tender.OperationUnknown {
		t.Fatalf("transport ambiguity recorded as a definite rejection: %v", err)
	}
	next := purchase()
	next.Key = "retry"
	if _, err := e.RetryCreate(t.Context(), r.Operation.ID, next); !errors.Is(err, tender.ErrConflict) || p.calls.Load() != 1 {
		t.Fatalf("retried ambiguous create: %v %d", err, p.calls.Load())
	}
}
