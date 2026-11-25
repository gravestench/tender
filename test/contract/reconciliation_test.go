//go:build integration

package contract_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/store/postgres"
)

// A changed comparison, report identity, or ledger lookup must make this test fail.
func TestReconcileReports(t *testing.T) {
	open := postgresFixture(t)
	db := open()
	defer db.Close()
	ctx := context.Background()
	scope := tender.Scope{MerchantID: "reconcile-shop", Environment: tender.EnvironmentMock}
	card := tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}
	ach := tender.Market{Country: "US", Currency: "USD", Method: tender.MethodACH}
	store := postgres.New(db)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	seed := func(id, ref string, market tender.Market, funds tender.Funds, status tender.PaymentStatus) {
		t.Helper()
		p, err := tender.NewPayment(scope, id, "order-"+id, tender.Money{Minor: 10000, Currency: "USD"})
		if err != nil {
			t.Fatal(err)
		}
		p.Provider, p.MerchantAccount, p.AttemptID, p.ProviderReference, p.Market = "adyen", "acct", "attempt-"+id, ref, market
		if _, err := p.Apply(tender.Snapshot{Status: status, Funds: funds}, now); err != nil {
			t.Fatal(err)
		}
		if err := store.Transact(ctx, func(tx tender.Tx) error { return tx.InsertPayment(p) }); err != nil {
			t.Fatal(err)
		}
	}
	seed("card", "psp-card", card, tender.Funds{Authorized: 10000, Captured: 10000, Refunded: 2500}, tender.PaymentCaptured)
	seed("bank", "psp-bank", ach, tender.Funds{Settled: 10000}, tender.PaymentSettled)
	seed("held-bank", "psp-held", ach, tender.Funds{Authorized: 10000, Settled: 10000}, tender.PaymentCaptureFailed)
	phantom := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: "phantom", PaymentID: "card", AttemptID: "attempt-card", Key: "phantom-key", Fingerprint: "fingerprint", ProviderKey: "provider-key", ProviderReference: "phantom-ref", Kind: tender.OpRefund, Status: tender.OperationSucceeded, Amount: tender.Money{Minor: 5000, Currency: "USD"}, BaseVersion: 2}}
	if err := store.Transact(ctx, func(tx tender.Tx) error { return tx.InsertOperation(phantom) }); err != nil {
		t.Fatal(err)
	}
	engine := func(m tender.Market) *tender.Engine {
		t.Helper()
		e, err := tender.NewEngine(tender.EngineConfig{Store: store, Provider: storageProvider{}, Scope: scope, ProviderName: "adyen", MerchantAccount: "acct", Market: m, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	cardEngine, bankEngine := engine(card), engine(ach)
	line := func(id, ref string, kind tender.SettlementEntryKind, gross, fee, net int64) tender.SettlementLine {
		return tender.SettlementLine{ID: id, ProviderReference: ref, Kind: kind, Currency: "USD", GrossMinor: gross, FeeMinor: fee, NetMinor: net, PayoutReference: "payout-1", ObservedAt: now}
	}
	cardReport := tender.SettlementReport{ID: "card-report", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{
		line("capture", "psp-card", tender.SettlementCaptured, 10000, 300, 9700),
		line("refund", "psp-card", tender.SettlementRefunded, -2500, 0, -2500),
		line("unmatched", "unknown", tender.SettlementCaptured, 10000, 300, 9700),
		line("wrong-currency", "psp-card", tender.SettlementCaptured, 10000, 300, 9700),
		line("wrong-amount", "psp-card", tender.SettlementCaptured, 9999, 299, 9700),
		line("wrong-fee", "psp-card", tender.SettlementCaptured, 10000, 299, 9700),
	}}
	cardReport.Lines[3].Currency = "EUR"
	run, err := cardEngine.Reconcile(ctx, cardReport)
	if err != nil {
		t.Fatal(err)
	}
	var orderedTimes bool
	if err := db.QueryRowContext(ctx, `SELECT finished_at >= started_at FROM tender_reconciliation_runs WHERE merchant_id=$1 AND run_id=$2`, scope.MerchantID, cardReport.ID).Scan(&orderedTimes); err != nil {
		t.Fatal(err)
	}
	if !orderedTimes {
		t.Fatal("reconciliation finished before it started")
	}
	if run.ID != cardReport.ID || len(run.Results) != 6 {
		t.Fatalf("run: %+v", run)
	}
	want := []tender.ReconciliationOutcome{tender.ReconciliationMatched, tender.ReconciliationMatched, tender.ReconciliationUnmatched, tender.ReconciliationCurrencyMismatch, tender.ReconciliationAmountMismatch, tender.ReconciliationFeeMismatch}
	for i, result := range run.Results {
		if result.Outcome != want[i] {
			t.Errorf("line %d: got %s want %s", i, result.Outcome, want[i])
		}
	}
	if run.Results[0].Line.FeeMinor != 300 || run.Results[0].Line.NetMinor != 9700 || run.Results[0].Line.PayoutReference != "payout-1" {
		t.Fatalf("missing payout evidence: %+v", run.Results[0])
	}
	phantomRun, err := cardEngine.Reconcile(ctx, tender.SettlementReport{ID: "phantom-report", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{line("phantom", "phantom-ref", tender.SettlementRefunded, -5000, 0, -5000)}})
	if err != nil {
		t.Fatal(err)
	}
	if phantomRun.Results[0].Outcome != tender.ReconciliationUnconfirmed {
		t.Fatalf("operation without ledger funds was matched: %+v", phantomRun.Results[0])
	}
	bankReport := tender.SettlementReport{ID: "ach-report", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{line("settled", "psp-bank", tender.SettlementSettled, 10000, 50, 9950)}}
	bankRun, err := bankEngine.Reconcile(ctx, bankReport)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range bankRun.Results {
		if result.Outcome != tender.ReconciliationMatched || result.PaymentID != "bank" {
			t.Errorf("bank result: %+v", result)
		}
	}
	if err := store.Transact(ctx, func(tx tender.Tx) error {
		p, err := tx.Payment(scope, "bank")
		if err != nil {
			return err
		}
		if _, err := p.Apply(tender.Snapshot{Status: tender.PaymentReturned, Funds: tender.Funds{Settled: 10000, Returned: 10000}}, now.Add(time.Hour)); err != nil {
			return err
		}
		return tx.UpdatePayment(p)
	}); err != nil {
		t.Fatal(err)
	}
	returnRun, err := bankEngine.Reconcile(ctx, tender.SettlementReport{ID: "ach-return", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{line("returned", "psp-bank", tender.SettlementReturned, -10000, 0, -10000)}})
	if err != nil {
		t.Fatal(err)
	}
	if returnRun.Results[0].Outcome != tender.ReconciliationMatched || returnRun.Results[0].PaymentID != "bank" {
		t.Fatalf("bank return: %+v", returnRun.Results[0])
	}
	held, err := bankEngine.Reconcile(ctx, tender.SettlementReport{ID: "held-report", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{line("held", "psp-held", tender.SettlementSettled, 10000, 50, 9950)}})
	if err != nil {
		t.Fatal(err)
	}
	if held.Results[0].Outcome != tender.ReconciliationUnconfirmed {
		t.Fatalf("capture failure hold was ignored: %+v", held.Results[0])
	}
	// A new engine and pool must replay the exact report without new history.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2 := open()
	defer db2.Close()
	restarted, err := tender.NewEngine(tender.EngineConfig{Store: postgres.New(db2), Provider: storageProvider{}, Scope: scope, ProviderName: "adyen", MerchantAccount: "acct", Market: card, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := restarted.Reconcile(ctx, cardReport)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay.Results) != len(run.Results) {
		t.Fatalf("duplicate result count: %d", len(replay.Results))
	}
	var historyCount, outboxCount int
	if err := db2.QueryRowContext(ctx, `SELECT count(*) FROM tender_reconciliation_history WHERE merchant_id=$1 AND run_id=$2`, scope.MerchantID, cardReport.ID).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if historyCount != 6 {
		t.Fatalf("history count %d", historyCount)
	}
	if err := db2.QueryRowContext(ctx, `SELECT count(*) FROM tender_outbox WHERE merchant_id=$1`, scope.MerchantID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 0 {
		t.Fatalf("reconciliation created %d outbox items", outboxCount)
	}
	// A report line can be revisited after the host ledger catches up. Retain
	// the original unmatched observation and append exactly one resolution.
	store2 := postgres.New(db2)
	p, err := tender.NewPayment(scope, "late", "order-late", tender.Money{Minor: 10000, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	p.Provider, p.MerchantAccount, p.AttemptID, p.ProviderReference, p.Market = "adyen", "acct", "attempt-late", "unknown", card
	if _, err := p.Apply(tender.Snapshot{Status: tender.PaymentCaptured, Funds: tender.Funds{Authorized: 10000, Captured: 10000}}, now); err != nil {
		t.Fatal(err)
	}
	if err := store2.Transact(ctx, func(tx tender.Tx) error { return tx.InsertPayment(p) }); err != nil {
		t.Fatal(err)
	}
	resolved, err := restarted.Reconcile(ctx, cardReport)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Results[2].Outcome != tender.ReconciliationMatched || resolved.Results[2].PaymentID != "late" {
		t.Fatalf("late resolution: %+v", resolved.Results[2])
	}
	if _, err := restarted.Reconcile(ctx, cardReport); err != nil {
		t.Fatal(err)
	}
	if err := db2.QueryRowContext(ctx, `SELECT count(*) FROM tender_reconciliation_history WHERE merchant_id=$1 AND run_id=$2`, scope.MerchantID, cardReport.ID).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if historyCount != 7 {
		t.Fatalf("duplicate or missing resolution history %d", historyCount)
	}
	changed := cardReport
	changed.Lines = append([]tender.SettlementLine(nil), cardReport.Lines...)
	changed.Lines[0].NetMinor--
	if _, err := restarted.Reconcile(ctx, changed); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("changed report: %v", err)
	}
	if err := db2.QueryRowContext(ctx, `SELECT count(*) FROM tender_reconciliation_history WHERE merchant_id=$1 AND run_id=$2 AND outcome='report_conflict'`, scope.MerchantID, cardReport.ID).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if historyCount != 1 {
		t.Fatalf("conflict history %d", historyCount)
	}
	openItems, err := store2.OpenReconciliationDiscrepancies(ctx, scope, "adyen", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if len(openItems) != 6 {
		t.Fatalf("open discrepancies: %+v", openItems)
	}
	for _, item := range openItems {
		if item.Result.Outcome == tender.ReconciliationMatched || item.Result.LineID == "unmatched" {
			t.Fatalf("resolved line remains open: %+v", item)
		}
	}
	// Operation references are unique only within one payment in the core
	// ledger, so two payments may expose the same PSP on this report route.
	for _, row := range []struct{ id, ref string }{{"collision-a", "psp-a"}, {"collision-b", "psp-b"}} {
		p, err := tender.NewPayment(scope, row.id, "order-"+row.id, tender.Money{Minor: 10000, Currency: "USD"})
		if err != nil {
			t.Fatal(err)
		}
		p.Provider, p.MerchantAccount, p.AttemptID, p.ProviderReference, p.Market = "adyen", "acct", "attempt-"+row.id, row.ref, card
		if err := store2.Transact(ctx, func(tx tender.Tx) error { return tx.InsertPayment(p) }); err != nil {
			t.Fatal(err)
		}
		op := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: "op-" + row.id, PaymentID: row.id, AttemptID: p.AttemptID, Key: "key-" + row.id, ProviderReference: "shared-operation-psp", Kind: tender.OpCapture, Status: tender.OperationSucceeded, Amount: tender.Money{Minor: 10000, Currency: "USD"}, BaseVersion: 1}}
		if err := store2.Transact(ctx, func(tx tender.Tx) error { return tx.InsertOperation(op) }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := restarted.Reconcile(ctx, tender.SettlementReport{ID: "shared-psp-report", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{line("shared", "shared-operation-psp", tender.SettlementCaptured, 10000, 0, 10000)}}); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("cross-payment operation PSP ambiguity: %v", err)
	}
	// A payment PSP may also equal a different payment's operation PSP.
	bridge := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: "op-bridge", PaymentID: "collision-a", AttemptID: "attempt-collision-a", Key: "key-bridge", ProviderReference: "psp-card", Kind: tender.OpCapture, Status: tender.OperationSucceeded, Amount: tender.Money{Minor: 10000, Currency: "USD"}, BaseVersion: 1}}
	if err := store2.Transact(ctx, func(tx tender.Tx) error { return tx.InsertOperation(bridge) }); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Reconcile(ctx, tender.SettlementReport{ID: "bridge-report", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{line("bridge", "psp-card", tender.SettlementCaptured, 10000, 0, 10000)}}); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("payment/operation PSP ambiguity: %v", err)
	}
	// Klarna confirmation deliberately binds its order ID to both the
	// payment and its confirm operation. This is one payment, not two claims.
	klarnaMarket := tender.Market{Country: "US", Currency: "USD", Method: tender.MethodKlarna}
	klarnaEngine, err := tender.NewEngine(tender.EngineConfig{Store: store2, Provider: storageProvider{}, Scope: scope, ProviderName: "klarna", MerchantAccount: "acct", Market: klarnaMarket})
	if err != nil {
		t.Fatal(err)
	}
	kp, err := tender.NewPayment(scope, "klarna", "order-klarna", tender.Money{Minor: 10000, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	kp.Provider, kp.MerchantAccount, kp.AttemptID, kp.ProviderReference, kp.Market = "klarna", "acct", "attempt-klarna", "klarna-order", klarnaMarket
	if _, err := kp.Apply(tender.Snapshot{Status: tender.PaymentCaptured, Funds: tender.Funds{Authorized: 10000, Captured: 10000}}, now); err != nil {
		t.Fatal(err)
	}
	if err := store2.Transact(ctx, func(tx tender.Tx) error { return tx.InsertPayment(kp) }); err != nil {
		t.Fatal(err)
	}
	kop := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: "klarna-confirm", PaymentID: kp.ID, AttemptID: kp.AttemptID, Key: "klarna-confirm-key", ProviderReference: kp.ProviderReference, Kind: tender.OpConfirm, Status: tender.OperationSucceeded, Amount: kp.Amount, BaseVersion: 1}}
	if err := store2.Transact(ctx, func(tx tender.Tx) error { return tx.InsertOperation(kop) }); err != nil {
		t.Fatal(err)
	}
	kLine := line("klarna-capture", "klarna-order", tender.SettlementCaptured, 10000, 0, 10000)
	kRun, err := klarnaEngine.Reconcile(ctx, tender.SettlementReport{ID: "klarna-same-order", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{kLine}})
	if err != nil {
		t.Fatal(err)
	}
	if kRun.Results[0].Outcome != tender.ReconciliationMatched || kRun.Results[0].PaymentID != kp.ID {
		t.Fatalf("same-payment Klarna reference: %+v", kRun.Results[0])
	}
	// A third claim from another payment must still make the reference
	// ambiguous even when the first two rows belong to the same payment.
	other, err := tender.NewPayment(scope, "klarna-other", "order-klarna-other", tender.Money{Minor: 10000, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	other.Provider, other.MerchantAccount, other.AttemptID, other.ProviderReference, other.Market = "klarna", "acct", "attempt-klarna-other", "other-order", klarnaMarket
	if err := store2.Transact(ctx, func(tx tender.Tx) error { return tx.InsertPayment(other) }); err != nil {
		t.Fatal(err)
	}
	otherOp := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: "klarna-other-confirm", PaymentID: other.ID, AttemptID: other.AttemptID, Key: "klarna-other-key", ProviderReference: "klarna-order", Kind: tender.OpConfirm, Status: tender.OperationSucceeded, Amount: other.Amount, BaseVersion: 1}}
	if err := store2.Transact(ctx, func(tx tender.Tx) error { return tx.InsertOperation(otherOp) }); err != nil {
		t.Fatal(err)
	}
	if _, err := klarnaEngine.Reconcile(ctx, tender.SettlementReport{ID: "klarna-cross-payment", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{kLine}}); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("cross-payment Klarna reference: %v", err)
	}
	// A real host timestamp carries monotonic and location state that JSONB
	// cannot preserve. Exact replay after reopening the pool must stay inert.
	monotonic := tender.SettlementReport{ID: "monotonic-report", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{line("clock", "unknown", tender.SettlementCaptured, 10000, 300, 9700)}}
	monotonic.Lines[0].ObservedAt = time.Now()
	if _, err := restarted.Reconcile(ctx, monotonic); err != nil {
		t.Fatal(err)
	}
	if err := db2.Close(); err != nil {
		t.Fatal(err)
	}
	db3 := open()
	defer db3.Close()
	reopened, err := tender.NewEngine(tender.EngineConfig{Store: postgres.New(db3), Provider: storageProvider{}, Scope: scope, ProviderName: "adyen", MerchantAccount: "acct", Market: card})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Reconcile(ctx, monotonic); err != nil {
		t.Fatal(err)
	}
	if err := db3.QueryRowContext(ctx, `SELECT count(*) FROM tender_reconciliation_history WHERE merchant_id=$1 AND run_id=$2`, scope.MerchantID, monotonic.ID).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if historyCount != 1 {
		t.Fatalf("exact monotonic-time replay added %d history rows", historyCount)
	}
	if _, err := bankEngine.Reconcile(ctx, tender.SettlementReport{ID: "unsupported", Feed: tender.ReportFeed("provider_auto")}); !errors.Is(err, tender.ErrUnsupported) {
		t.Fatalf("unsupported feed: %v", err)
	}
}
