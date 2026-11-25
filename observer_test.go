package tender_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/store"
)

type observerFunc func(context.Context, tender.OperationalEvent) error

func (f observerFunc) Observe(ctx context.Context, e tender.OperationalEvent) error { return f(ctx, e) }

func waitOperationalEvent(t *testing.T, events <-chan tender.OperationalEvent) tender.OperationalEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("observer event was not delivered")
		return tender.OperationalEvent{}
	}
}

func observedEngine(t *testing.T, db tender.Store, p tender.Provider, observer tender.Observer) *tender.Engine {
	t.Helper()
	e, err := tender.NewEngine(tender.EngineConfig{Store: db, Provider: p, Scope: shop, ProviderName: "acquirer", MerchantAccount: "merchant-account", Market: usVisa, Observer: observer})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestOperationalObserverBlockedSinkCannotHoldPayment(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 1)
	e := observedEngine(t, store.NewMemory(), &processor{execute: authorized}, observerFunc(func(context.Context, tender.OperationalEvent) error {
		entered <- struct{}{}
		<-release
		return nil
	}))
	done := make(chan response, 1)
	go func() { result, err := e.Create(t.Context(), purchase()); done <- response{result, err} }()
	select {
	case got := <-done:
		if got.err != nil || got.result.Payment.Status != tender.PaymentAuthorized {
			t.Fatalf("payment changed: %+v", got)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("observer blocked payment result after commit")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("observer did not receive event")
	}
}

func TestOperationalObserverProviderResult(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider func(context.Context, tender.ProviderRequest) (tender.Observation, error)
		want     tender.ErrorKind
	}{
		{"success", authorized, ""},
		{"rate_limit", func(context.Context, tender.ProviderRequest) (tender.Observation, error) {
			return tender.Observation{}, &tender.ProviderError{Kind: tender.ErrRateLimit}
		}, tender.ErrRateLimit},
		{"unknown", func(context.Context, tender.ProviderRequest) (tender.Observation, error) {
			return tender.Observation{}, context.DeadlineExceeded
		}, tender.ErrUnknownOutcome},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := make(chan tender.OperationalEvent, 1)
			observer := observerFunc(func(_ context.Context, e tender.OperationalEvent) error { events <- e; return nil })
			db := store.NewMemory()
			e := observedEngine(t, db, &processor{execute: tc.provider}, observer)
			result, _ := e.Create(t.Context(), purchase())
			got := waitOperationalEvent(t, events)
			if got.Kind != tender.OperationalProviderCall || got.ErrorKind != tc.want || got.Scope != shop || got.PaymentID != "payment" || got.OperationID != result.Operation.ID || got.Provider != "acquirer" || got.MerchantAccount != "merchant-account" || got.At.IsZero() || got.At.Location() != time.UTC || got.Duration < 0 {
				t.Fatalf("event = %+v; result = %+v", got, result)
			}
		})
	}
}

func TestOperationalObserverFailureDoesNotChangePayment(t *testing.T) {
	db := store.NewMemory()
	e := observedEngine(t, db, &processor{execute: authorized}, observerFunc(func(context.Context, tender.OperationalEvent) error { panic("host observer failed") }))
	result, err := e.Create(t.Context(), purchase())
	if err != nil || result.Payment.Status != tender.PaymentAuthorized {
		t.Fatalf("create = %+v, %v", result, err)
	}
	got, err := e.Get(t.Context(), "payment")
	if err != nil || got.Status != tender.PaymentAuthorized {
		t.Fatalf("payment = %+v, %v", got, err)
	}
}

func TestOperationalObserverCanceledCaller(t *testing.T) {
	db := store.NewMemory()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := make(chan tender.OperationalEvent, 1)
	e := observedEngine(t, db, &processor{execute: func(call context.Context, _ tender.ProviderRequest) (tender.Observation, error) {
		cancel()
		return tender.Observation{}, call.Err()
	}}, observerFunc(func(observerCtx context.Context, event tender.OperationalEvent) error {
		if observerCtx.Err() != nil {
			t.Error("observer received canceled context")
		}
		events <- event
		return nil
	}))
	result, err := e.Create(ctx, purchase())
	if result.Operation.Status != tender.OperationUnknown || !errors.Is(err, tender.ErrUnknownOutcome) {
		t.Fatalf("canceled dispatched create = %+v, %v", result, err)
	}
	if got := waitOperationalEvent(t, events); got.ErrorKind != tender.ErrUnknownOutcome {
		t.Fatalf("event = %+v", got)
	}
}

func TestOperationalObserverWebhookLagAndReplay(t *testing.T) {
	db := store.NewMemory()
	var req tender.ProviderRequest
	events := make(chan tender.OperationalEvent, 2)
	var e *tender.Engine
	observer := observerFunc(func(ctx context.Context, event tender.OperationalEvent) error {
		if event.Kind == tender.OperationalWebhookReceipt {
			if _, err := e.Get(ctx, "payment"); err != nil {
				t.Errorf("observer ran under store lock: %v", err)
			}
		}
		events <- event
		return errors.New("observer is best effort")
	})
	e = observedEngine(t, db, &processor{execute: func(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
		req = r
		return observed(r, tender.PaymentPending, tender.Funds{}), nil
	}}, observer)
	if _, err := e.Create(t.Context(), purchase()); err != nil {
		t.Fatal(err)
	}
	_ = waitOperationalEvent(t, events)
	key := tender.VerifiedEventKey{Scope: shop, Provider: "acquirer", MerchantAccount: "merchant-account", ID: "receipt"}
	if err := db.Transact(t.Context(), func(tx tender.Tx) error {
		return tx.(tender.VerifiedEventTx).InsertVerifiedEvent(key, json.RawMessage(`{"buyer":"secret-buyer","token":"secret-token"}`))
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	project := func(json.RawMessage) (tender.VerifiedEvent, error) {
		return tender.VerifiedEvent{ID: "receipt", Provider: "acquirer", Observation: observed(req, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100})}, nil
	}
	if err := e.ProcessVerifiedEvent(t.Context(), "receipt", project); err != nil {
		t.Fatal(err)
	}
	if err := e.ProcessVerifiedEvent(t.Context(), "receipt", project); err != nil {
		t.Fatal(err)
	}
	gotEvent := waitOperationalEvent(t, events)
	if gotEvent.Kind != tender.OperationalWebhookReceipt || gotEvent.ReceiptLag <= 0 || gotEvent.PaymentID != "payment" || gotEvent.OperationID != req.Operation.ID {
		t.Fatalf("event = %+v", gotEvent)
	}
	out, err := db.Outbox(t.Context(), shop)
	if err != nil || len(out) != 1 {
		t.Fatalf("outbox after observer error = %+v, %v", out, err)
	}
	b, _ := json.Marshal(gotEvent)
	if strings.Contains(string(b), "secret-buyer") || strings.Contains(string(b), "secret-token") || strings.Contains(string(b), `"receipt"`) {
		t.Fatalf("event leaked raw receipt: %s", b)
	}
}

type reconciliationStore struct {
	tender.Store
	conflict bool
}
type reconciliationTx struct {
	tender.Tx
	conflict bool
}

func (s reconciliationStore) Transact(ctx context.Context, fn func(tender.Tx) error) error {
	return s.Store.Transact(ctx, func(tx tender.Tx) error { return fn(reconciliationTx{tx, s.conflict}) })
}
func (tx reconciliationTx) ReconciliationRun(tender.Scope, string, string, string) (tender.ReconciliationRun, error) {
	if tx.conflict {
		return tender.ReconciliationRun{ID: "sensitive-report-id", Digest: "different-digest"}, nil
	}
	return tender.ReconciliationRun{}, tender.ErrNotFound
}
func (reconciliationTx) InsertReconciliationRun(tender.Scope, string, string, tender.ReconciliationRun) error {
	return nil
}
func (reconciliationTx) InsertSettlementLine(tender.Scope, string, string, string, tender.SettlementLine) error {
	return nil
}
func (reconciliationTx) ResolveSettlementReference(tender.Scope, string, string, string) (tender.Payment, tender.OperationRecord, error) {
	return tender.Payment{}, tender.OperationRecord{}, tender.ErrNotFound
}
func (reconciliationTx) AppendReconciliationResult(tender.Scope, string, string, string, tender.ReconciliationResult) error {
	return nil
}
func (reconciliationTx) FinishReconciliationRun(tender.Scope, string, string, string) error {
	return nil
}
func (reconciliationTx) ReconciliationResults(tender.Scope, string, string, string) ([]tender.ReconciliationResult, error) {
	return nil, nil
}
func (reconciliationTx) InsertReconciliationPayoutInput(tender.Scope, string, string, string, tender.SettlementReport) error {
	return nil
}
func (reconciliationTx) AppendPayoutReconciliationResult(tender.Scope, string, string, string, tender.PayoutReconciliationResult) error {
	return nil
}
func (reconciliationTx) PayoutReconciliationResults(tender.Scope, string, string, string) ([]tender.PayoutReconciliationResult, error) {
	return nil, nil
}

func TestOperationalObserverReconciliationDiscrepancy(t *testing.T) {
	events := make(chan tender.OperationalEvent, 1)
	var e *tender.Engine
	observer := observerFunc(func(ctx context.Context, event tender.OperationalEvent) error {
		if event.Kind == tender.OperationalReconciliation {
			if _, err := e.Get(ctx, "payment"); !errors.Is(err, tender.ErrNotFound) {
				t.Errorf("observer ran within store transaction: %v", err)
			}
		}
		events <- event
		return nil
	})
	e = observedEngine(t, reconciliationStore{Store: store.NewMemory()}, &processor{execute: authorized}, observer)
	report := tender.SettlementReport{ID: "sensitive-report-id", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{{ID: "sensitive-row-id", ProviderReference: "unmatched", Currency: "USD", Kind: tender.SettlementCaptured, GrossMinor: 100, FeeMinor: 0, NetMinor: 100, ObservedAt: time.Now().UTC()}}}
	run, err := e.Reconcile(t.Context(), report)
	if err != nil || len(run.Results) != 1 || run.Results[0].Outcome != tender.ReconciliationUnmatched {
		t.Fatalf("reconcile = %+v, %v", run, err)
	}
	gotEvent := waitOperationalEvent(t, events)
	if gotEvent.Kind != tender.OperationalReconciliation || gotEvent.Discrepancies != 1 || gotEvent.PayoutDiscrepancies != 0 {
		t.Fatalf("event = %+v", gotEvent)
	}
	b, _ := json.Marshal(gotEvent)
	if strings.Contains(string(b), "sensitive-report-id") || strings.Contains(string(b), "sensitive-row-id") {
		t.Fatalf("event leaked report evidence: %s", b)
	}
}

func TestOperationalObserverNoticeOnlyReceipt(t *testing.T) {
	db := store.NewMemory()
	p, err := tender.NewPayment(shop, "payment", "order", usd100)
	if err != nil {
		t.Fatal(err)
	}
	p.Provider, p.MerchantAccount, p.Market, p.AttemptID, p.ProviderReference = "acquirer", "merchant-account", usVisa, "attempt", "payment-psp"
	if _, err := p.Apply(tender.Snapshot{Status: tender.PaymentCaptured, VerifiedMethod: tender.MethodVisa, Funds: tender.Funds{Authorized: 100, Captured: 100}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.Transact(t.Context(), func(tx tender.Tx) error { return tx.InsertPayment(p) }); err != nil {
		t.Fatal(err)
	}
	events := make(chan tender.OperationalEvent, 1)
	e := observedEngine(t, db, &processor{execute: authorized}, observerFunc(func(_ context.Context, event tender.OperationalEvent) error {
		events <- event
		return errors.New("host sink down")
	}))
	key := tender.VerifiedEventKey{Scope: shop, Provider: "acquirer", MerchantAccount: "merchant-account", ID: "dispute"}
	if err := db.Transact(t.Context(), func(tx tender.Tx) error {
		return tx.(tender.VerifiedEventTx).InsertVerifiedEvent(key, json.RawMessage(`{"reason":"secret"}`))
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	project := func(tender.EventRecords, json.RawMessage) (tender.AdverseNotice, error) {
		return tender.AdverseNotice{Scope: shop, Provider: "acquirer", MerchantAccount: "merchant-account", PaymentID: "payment", ProviderEventID: "dispute", Kind: "chargeback", OriginalReference: "payment-psp", ModificationReference: "dispute-psp", Amount: usd100, ObservedAt: time.Now().UTC()}, nil
	}
	if err := e.ProcessVerifiedAdverseEvent(t.Context(), "dispute", project); err != nil {
		t.Fatal(err)
	}
	if err := e.ProcessVerifiedAdverseEvent(t.Context(), "dispute", project); err != nil {
		t.Fatal(err)
	}
	gotEvent := waitOperationalEvent(t, events)
	if gotEvent.Kind != tender.OperationalWebhookReceipt || gotEvent.PaymentID != "payment" || gotEvent.ReceiptLag <= 0 {
		t.Fatalf("event = %+v", gotEvent)
	}
	out, err := db.Outbox(t.Context(), shop)
	if err != nil || len(out) != 0 {
		t.Fatalf("notice changed outbox: %+v, %v", out, err)
	}
}
