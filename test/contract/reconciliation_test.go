//go:build integration

package contract_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
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
	returnLine := line("returned", "psp-bank", tender.SettlementReturned, -10000, 0, -10000)
	returnLine.OriginalReference, returnLine.ModificationReference = "psp-bank", "return-psp"
	returnReport := tender.SettlementReport{ID: "ach-return", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{returnLine}}
	returnRun, err := bankEngine.Reconcile(ctx, returnReport)
	if err != nil {
		t.Fatal(err)
	}
	if returnRun.Results[0].Outcome != tender.ReconciliationUnconfirmed || returnRun.Results[0].PaymentID != "bank" {
		t.Fatalf("report alone confirmed a bank return: %+v", returnRun.Results[0])
	}
	notice := tender.AdverseNotice{Scope: scope, Provider: "adyen", MerchantAccount: "acct", PaymentID: "bank", ProviderEventID: "return-event", Kind: "returned", OriginalReference: "psp-bank", ModificationReference: "return-psp", Amount: tender.Money{Minor: 10000, Currency: "USD"}, ObservedAt: now}
	rawNotice, _ := json.Marshal(notice)
	if err := store.Transact(ctx, func(tx tender.Tx) error {
		return tx.(tender.VerifiedEventTx).InsertVerifiedEvent(tender.VerifiedEventKey{Scope: scope, Provider: "adyen", MerchantAccount: "acct", ID: "return-event"}, rawNotice)
	}); err != nil {
		t.Fatal(err)
	}
	if err := bankEngine.ProcessVerifiedAdverseEvent(ctx, "return-event", func(_ tender.EventRecords, raw json.RawMessage) (tender.AdverseNotice, error) {
		var n tender.AdverseNotice
		err := json.Unmarshal(raw, &n)
		return n, err
	}); err != nil {
		t.Fatal(err)
	}
	verifiedReturn, err := bankEngine.Reconcile(ctx, returnReport)
	if err != nil || verifiedReturn.Results[0].Outcome != tender.ReconciliationMatched {
		t.Fatalf("processed verified return remained open: %+v, %v", verifiedReturn.Results, err)
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

// A changed batch sum, missing source part, or cross-payment modification
// must make this test fail. The report itself never changes payment funds.
func TestReconcilePayoutBatches(t *testing.T) {
	open := postgresFixture(t)
	db := open()
	defer db.Close()
	ctx := context.Background()
	scope := tender.Scope{MerchantID: "payout-shop", Environment: tender.EnvironmentMock}
	market := tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}
	store := postgres.New(db)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	for _, seed := range []struct {
		id, paymentRef, operationRef string
		kind                         tender.OperationKind
		amount                       int64
	}{
		{"sale", "psp-sale", "mod-sale", tender.OpCapture, 10000},
		{"refund", "psp-refund", "mod-refund", tender.OpRefund, 2500},
	} {
		p, err := tender.NewPayment(scope, seed.id, "order-"+seed.id, tender.Money{Minor: 10000, Currency: "USD"})
		if err != nil {
			t.Fatal(err)
		}
		p.Provider, p.MerchantAccount, p.Market, p.AttemptID, p.ProviderReference = "adyen", "acct", market, "attempt-"+seed.id, seed.paymentRef
		funds := tender.Funds{Authorized: 10000, Captured: 10000}
		if seed.kind == tender.OpRefund {
			funds.Refunded = 2500
		}
		if _, err := p.Apply(tender.Snapshot{Status: tender.PaymentCaptured, Funds: funds}, now); err != nil {
			t.Fatal(err)
		}
		op := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: "op-" + seed.id, PaymentID: seed.id, AttemptID: p.AttemptID, Key: "key-" + seed.id, ProviderReference: seed.operationRef, Kind: seed.kind, Status: tender.OperationSucceeded, Amount: tender.Money{Minor: seed.amount, Currency: "USD"}, BaseVersion: 1}}
		if err := store.Transact(ctx, func(tx tender.Tx) error {
			if err := tx.InsertPayment(p); err != nil {
				return err
			}
			return tx.InsertOperation(op)
		}); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := tender.NewEngine(tender.EngineConfig{Store: store, Provider: storageProvider{}, Scope: scope, ProviderName: "adyen", MerchantAccount: "acct", Market: market})
	if err != nil {
		t.Fatal(err)
	}
	line := func(id, original, modification string, kind tender.SettlementEntryKind, gross, fee, net int64) tender.SettlementLine {
		return tender.SettlementLine{ID: id, ProviderReference: modification, OriginalReference: original, ModificationReference: modification, BatchID: "batch-1", SourcePartID: "part-1", SourceSectionID: "section-1", Kind: kind, Currency: "USD", GrossMinor: gross, FeeMinor: fee, NetMinor: net, ObservedAt: now}
	}
	net, payout := int64(7100), int64(7100)
	report := tender.SettlementReport{
		ID: "complete-batch", Feed: tender.FeedHostVerified,
		Lines: []tender.SettlementLine{
			line("sale-row", "psp-sale", "mod-sale", tender.SettlementCaptured, 10000, 300, 9700),
			line("refund-row", "psp-refund", "mod-refund", tender.SettlementRefunded, -2500, 0, -2500),
		},
		Adjustments: []tender.PayoutAdjustment{{ID: "reserve", BatchID: "batch-1", SourcePartID: "part-1", SourceSectionID: "section-1", Currency: "USD", Type: "reserve", SignedMinor: -100}},
		Payouts:     []tender.PayoutLine{{ID: "payout-row", BatchID: "batch-1", SourcePartID: "part-1", SourceSectionID: "section-1", Currency: "USD", PayoutReference: "bank-payout-1", AmountMinor: 7100, ObservedAt: now}},
		Parts:       []tender.ReportPart{{ID: "part-1", SectionID: "section-1", Digest: "auth-artifact-sha256", Number: 1, TotalParts: 1, DeclaredRows: 4}},
		Sections:    []tender.ReportSection{{ID: "section-1", BatchID: "batch-1", Currency: "USD", Number: 1, TotalSections: 1, DeclaredRows: 4, DeclaredNetMinor: &net, DeclaredPayoutMinor: &payout}},
	}
	run, err := engine.Reconcile(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Results) != 2 || run.Results[0].Outcome != tender.ReconciliationMatched || run.Results[1].Outcome != tender.ReconciliationMatched {
		t.Fatalf("transaction results: %+v", run.Results)
	}
	if len(run.PayoutResults) != 1 || run.PayoutResults[0].Outcome != tender.PayoutMatched || run.PayoutResults[0].ExpectedMinor != 7100 || run.PayoutResults[0].BatchReportedMinor != 7100 {
		t.Fatalf("payout result: %+v", run.PayoutResults)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = open()
	store = postgres.New(db)
	restarted, err := tender.NewEngine(tender.EngineConfig{Store: store, Provider: storageProvider{}, Scope: scope, ProviderName: "adyen", MerchantAccount: "acct", Market: market})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Reconcile(ctx, report); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM tender_reconciliation_payout_history WHERE merchant_id=$1 AND run_id=$2`, scope.MerchantID, report.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("replay payout history count %d: %v", count, err)
	}
	badOriginal := report
	badOriginal.ID = "wrong-original"
	badOriginal.Lines = append([]tender.SettlementLine(nil), report.Lines...)
	badOriginal.Lines[0].OriginalReference = "psp-refund"
	badRun, err := restarted.Reconcile(ctx, badOriginal)
	if err != nil {
		t.Fatal(err)
	}
	if badRun.Results[0].Outcome == tender.ReconciliationMatched || badRun.PayoutResults[0].Outcome == tender.PayoutMatched {
		t.Fatalf("cross-payment original reference matched: %+v", badRun)
	}
	wrongOriginalKind := report
	wrongOriginalKind.ID = "wrong-original-kind"
	wrongOriginalKind.Lines = append([]tender.SettlementLine(nil), report.Lines...)
	wrongOriginalKind.Lines[0].OriginalReference = "mod-sale" // an operation, not the original payment
	wrongKindRun, err := restarted.Reconcile(ctx, wrongOriginalKind)
	if err != nil || wrongKindRun.Results[0].Outcome == tender.ReconciliationMatched {
		t.Fatalf("operation accepted as original payment reference: %+v, %v", wrongKindRun.Results, err)
	}
	missingModification := report
	missingModification.ID = "missing-capture-modification"
	missingModification.Lines = append([]tender.SettlementLine(nil), report.Lines...)
	missingModification.Lines[0].ProviderReference = "psp-sale"
	missingModification.Lines[0].ModificationReference = ""
	missingModificationRun, err := restarted.Reconcile(ctx, missingModification)
	if err != nil || missingModificationRun.Results[0].Outcome != tender.ReconciliationUnconfirmed || missingModificationRun.PayoutResults[0].Outcome != tender.PayoutUnconfirmed {
		t.Fatalf("capture without exact modification matched: %+v, %v", missingModificationRun, err)
	}
	unlinked := report
	unlinked.ID = "unlinked-row"
	unlinked.Lines = append([]tender.SettlementLine(nil), report.Lines...)
	extra := line("unlinked", "psp-sale", "mod-sale", tender.SettlementCaptured, 10000, 300, 9700)
	extra.SourcePartID, extra.SourceSectionID = "", ""
	unlinked.Lines = append(unlinked.Lines, extra)
	unlinkedRun, err := restarted.Reconcile(ctx, unlinked)
	if err == nil && unlinkedRun.PayoutResults[0].Outcome == tender.PayoutMatched {
		t.Fatalf("unattributed report row did not block payout: %+v", unlinkedRun.PayoutResults)
	}
	orphan := report
	orphan.ID = "orphan-part"
	orphan.Parts = append([]tender.ReportPart(nil), report.Parts...)
	orphan.Parts = append(orphan.Parts, tender.ReportPart{ID: "orphan", SectionID: "missing-section", Digest: "authenticated-orphan", Number: 1, TotalParts: 1, DeclaredRows: 0})
	if _, err := restarted.Reconcile(ctx, orphan); !errors.Is(err, tender.ErrValidation) {
		t.Fatalf("orphan source part: %v", err)
	}
	overflow := report
	overflow.ID = "overflow-batch"
	overflow.Lines = append([]tender.SettlementLine(nil), report.Lines...)
	overflow.Lines[0].ProviderReference, overflow.Lines[0].OriginalReference, overflow.Lines[0].ModificationReference = "unknown-overflow", "", ""
	overflow.Lines[0].GrossMinor, overflow.Lines[0].FeeMinor, overflow.Lines[0].NetMinor = math.MaxInt64, 0, math.MaxInt64
	overflow.Lines = overflow.Lines[:1]
	overflow.Adjustments = append([]tender.PayoutAdjustment(nil), report.Adjustments...)
	overflow.Adjustments[0].SignedMinor = 1
	overflow.Parts = append([]tender.ReportPart(nil), report.Parts...)
	overflow.Parts[0].DeclaredRows = 3
	overflow.Sections = append([]tender.ReportSection(nil), report.Sections...)
	overflow.Sections[0].DeclaredRows = 3
	if _, err := restarted.Reconcile(ctx, overflow); !errors.Is(err, tender.ErrValidation) {
		t.Fatalf("overflowing batch sum: %v", err)
	}
	partial := report
	partial.ID = "missing-part"
	partial.Parts = append([]tender.ReportPart(nil), report.Parts...)
	partial.Parts[0].TotalParts = 2
	partialRun, err := restarted.Reconcile(ctx, partial)
	if err != nil || partialRun.PayoutResults[0].Outcome != tender.PayoutIncomplete {
		t.Fatalf("incomplete source artifact: %+v, %v", partialRun.PayoutResults, err)
	}
	duplicateArtifact := report
	duplicateArtifact.ID = "duplicate-artifact"
	duplicateArtifact.Parts = []tender.ReportPart{
		{ID: "part-1", SectionID: "section-1", Digest: "same-authenticated-file", Number: 1, TotalParts: 2, DeclaredRows: 4},
		{ID: "part-2", SectionID: "section-1", Digest: "same-authenticated-file", Number: 2, TotalParts: 2, DeclaredRows: 0},
	}
	if _, err := restarted.Reconcile(ctx, duplicateArtifact); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("same provider artifact counted twice: %v", err)
	}
	wrongCurrency := report
	wrongCurrency.ID = "wrong-payout-currency"
	wrongCurrency.Payouts = append([]tender.PayoutLine(nil), report.Payouts...)
	wrongCurrency.Payouts[0].Currency = "EUR"
	currencyRun, err := restarted.Reconcile(ctx, wrongCurrency)
	if err != nil || currencyRun.PayoutResults[0].Outcome != tender.PayoutCurrencyMismatch {
		t.Fatalf("payout currency discrepancy: %+v, %v", currencyRun.PayoutResults, err)
	}
	wrongProviderTotal := report
	wrongProviderTotal.ID = "wrong-provider-total"
	wrongProviderTotal.Sections = append([]tender.ReportSection(nil), report.Sections...)
	wrongNet := int64(7101)
	wrongProviderTotal.Sections[0].DeclaredNetMinor = &wrongNet
	totalRun, err := restarted.Reconcile(ctx, wrongProviderTotal)
	if err != nil || totalRun.PayoutResults[0].Outcome != tender.PayoutProviderTotalMismatch {
		t.Fatalf("provider section total discrepancy: %+v, %v", totalRun.PayoutResults, err)
	}
	openPayouts, err := store.OpenPayoutDiscrepancies(ctx, scope, "adyen", "acct")
	if err != nil {
		t.Fatal(err)
	}
	var foundMismatch, foundMatched bool
	for _, item := range openPayouts {
		if item.ReportID == wrongProviderTotal.ID && item.Result.Outcome == tender.PayoutProviderTotalMismatch {
			foundMismatch = true
		}
		if item.ReportID == report.ID {
			foundMatched = true
		}
	}
	if !foundMismatch || foundMatched {
		t.Fatalf("open payout discrepancies: %+v", openPayouts)
	}
	wrongPayout := report
	wrongPayout.ID = "wrong-payout-amount"
	wrongPayout.Payouts = append([]tender.PayoutLine(nil), report.Payouts...)
	wrongPayout.Payouts[0].AmountMinor = 7099
	wrongPayout.Sections = append([]tender.ReportSection(nil), report.Sections...)
	wrongPayoutAmount := int64(7099)
	wrongPayout.Sections[0].DeclaredPayoutMinor = &wrongPayoutAmount
	amountRun, err := restarted.Reconcile(ctx, wrongPayout)
	if err != nil || amountRun.PayoutResults[0].Outcome != tender.PayoutAmountMismatch {
		t.Fatalf("batch payout amount discrepancy: %+v, %v", amountRun.PayoutResults, err)
	}
	wrongRowCount := report
	wrongRowCount.ID = "wrong-row-count"
	wrongRowCount.Sections = append([]tender.ReportSection(nil), report.Sections...)
	wrongRowCount.Sections[0].DeclaredRows = 5
	rowRun, err := restarted.Reconcile(ctx, wrongRowCount)
	if err != nil || rowRun.PayoutResults[0].Outcome != tender.PayoutIncomplete {
		t.Fatalf("declared row count discrepancy: %+v, %v", rowRun.PayoutResults, err)
	}
	split := report
	split.ID = "two-section-batch"
	split.Lines = append([]tender.SettlementLine(nil), report.Lines...)
	split.Adjustments = append([]tender.PayoutAdjustment(nil), report.Adjustments...)
	split.Parts = []tender.ReportPart{
		{ID: "part-1", SectionID: "section-1", Digest: "auth-artifact-sha256", Number: 1, TotalParts: 1, DeclaredRows: 3},
		{ID: "part-2", SectionID: "section-2", Digest: "auth-artifact-2-sha256", Number: 1, TotalParts: 1, DeclaredRows: 1},
	}
	split.Adjustments[0].SourcePartID, split.Adjustments[0].SourceSectionID = "part-2", "section-2"
	sectionOneNet, sectionTwoNet, sectionTwoPayout := int64(7200), int64(-100), int64(0)
	split.Sections = []tender.ReportSection{
		{ID: "section-1", BatchID: "batch-1", Currency: "USD", Number: 1, TotalSections: 2, DeclaredRows: 3, DeclaredNetMinor: &sectionOneNet, DeclaredPayoutMinor: &payout},
		{ID: "section-2", BatchID: "batch-1", Currency: "USD", Number: 2, TotalSections: 2, DeclaredRows: 1, DeclaredNetMinor: &sectionTwoNet, DeclaredPayoutMinor: &sectionTwoPayout},
	}
	splitRun, err := restarted.Reconcile(ctx, split)
	if err != nil || splitRun.PayoutResults[0].Outcome != tender.PayoutMatched || splitRun.PayoutResults[0].ExpectedMinor != 7100 {
		t.Fatalf("two-section batch total: %+v, %v", splitRun.PayoutResults, err)
	}
	missingSectionRow := split
	missingSectionRow.ID = "missing-section-row"
	missingSectionRow.Adjustments = nil
	missingRun, err := restarted.Reconcile(ctx, missingSectionRow)
	if err != nil || missingRun.PayoutResults[0].Outcome != tender.PayoutIncomplete {
		t.Fatalf("incomplete other section matched payout: %+v, %v", missingRun.PayoutResults, err)
	}
	payoutOnly := tender.SettlementReport{ID: "payout-only", Feed: tender.FeedHostVerified, Payouts: []tender.PayoutLine{{ID: "solo", BatchID: "batch-solo", Currency: "USD", PayoutReference: "bank-solo", AmountMinor: 100, ObservedAt: now}}}
	payoutOnlyRun, err := restarted.Reconcile(ctx, payoutOnly)
	if err != nil || len(payoutOnlyRun.PayoutResults) != 1 || payoutOnlyRun.PayoutResults[0].Outcome != tender.PayoutIncomplete {
		t.Fatalf("payout-only must remain open: %+v, %v", payoutOnlyRun.PayoutResults, err)
	}
	adjustmentOnly := tender.SettlementReport{ID: "adjustment-only", Feed: tender.FeedHostVerified, Adjustments: []tender.PayoutAdjustment{{ID: "fee", BatchID: "batch-solo", Currency: "USD", Type: "fee", SignedMinor: -50}}}
	adjustmentRun, err := restarted.Reconcile(ctx, adjustmentOnly)
	if err != nil || len(adjustmentRun.PayoutResults) != 1 || adjustmentRun.PayoutResults[0].Outcome != tender.PayoutMissing {
		t.Fatalf("adjustment-only must remain open: %+v, %v", adjustmentRun.PayoutResults, err)
	}
	duplicatePayoutID := report
	duplicatePayoutID.ID = "duplicate-payout-id"
	duplicatePayoutID.Payouts = append(append([]tender.PayoutLine(nil), report.Payouts...), report.Payouts[0])
	if _, err := restarted.Reconcile(ctx, duplicatePayoutID); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("duplicate payout row identity: %v", err)
	}
	otherScope := tender.Scope{MerchantID: "another-payout-shop", Environment: tender.EnvironmentMock}
	otherEngine, err := tender.NewEngine(tender.EngineConfig{Store: store, Provider: storageProvider{}, Scope: otherScope, ProviderName: "adyen", MerchantAccount: "acct", Market: market})
	if err != nil {
		t.Fatal(err)
	}
	otherRun, err := otherEngine.Reconcile(ctx, report)
	if err != nil || len(otherRun.PayoutResults) != 1 || otherRun.PayoutResults[0].Outcome == tender.PayoutMatched {
		t.Fatalf("cross-merchant batch isolation: %+v, %v", otherRun.PayoutResults, err)
	}
	changed := report
	changed.Payouts = append([]tender.PayoutLine(nil), report.Payouts...)
	changed.Payouts[0].AmountMinor--
	if _, err := restarted.Reconcile(ctx, changed); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("changed report identity: %v", err)
	}
	sharedPayment, err := tender.NewPayment(scope, "shared-ref", "order-shared-ref", tender.Money{Minor: 10000, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	sharedPayment.Provider, sharedPayment.MerchantAccount, sharedPayment.Market, sharedPayment.AttemptID, sharedPayment.ProviderReference = "adyen", "acct", market, "attempt-shared-ref", "same-payment-ref"
	if _, err := sharedPayment.Apply(tender.Snapshot{Status: tender.PaymentCaptured, Funds: tender.Funds{Authorized: 10000, Captured: 10000}}, now); err != nil {
		t.Fatal(err)
	}
	sharedOperation := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: "shared-ref-capture", PaymentID: sharedPayment.ID, AttemptID: sharedPayment.AttemptID, Key: "shared-ref-key", ProviderReference: sharedPayment.ProviderReference, Kind: tender.OpCapture, Status: tender.OperationSucceeded, Amount: sharedPayment.Amount, BaseVersion: 1}}
	if err := store.Transact(ctx, func(tx tender.Tx) error {
		if err := tx.InsertPayment(sharedPayment); err != nil {
			return err
		}
		return tx.InsertOperation(sharedOperation)
	}); err != nil {
		t.Fatal(err)
	}
	sharedLine := line("shared-ref-row", "same-payment-ref", "same-payment-ref", tender.SettlementCaptured, 10000, 0, 10000)
	sharedRun, err := restarted.Reconcile(ctx, tender.SettlementReport{ID: "same-payment-reference", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{sharedLine}})
	if err != nil || sharedRun.Results[0].Outcome != tender.ReconciliationMatched {
		t.Fatalf("same-payment exact capture operation: %+v, %v", sharedRun.Results, err)
	}
	var outboxCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM tender_outbox WHERE merchant_id=$1`, scope.MerchantID).Scan(&outboxCount); err != nil || outboxCount != 0 {
		t.Fatalf("reconcile changed fulfillment outbox: %d, %v", outboxCount, err)
	}
}
