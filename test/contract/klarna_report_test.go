//go:build integration

package contract_test

import (
	"context"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/providers/klarna"
	"github.com/gravestench/tender/store/postgres"
)

// A missing operation, repeated capture, or incomplete payout must not match.
func TestKlarnaReportReconcile(t *testing.T) {
	open := postgresFixture(t)
	db := open()
	defer db.Close()
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	scope := tender.Scope{MerchantID: "klarna-report-shop", Environment: tender.EnvironmentMock}
	market := tender.Market{Country: "US", Currency: "USD", Method: tender.MethodKlarna}
	store := postgres.New(db)
	p, err := tender.NewPayment(scope, "payment-1", "cart-1", tender.Money{Minor: 35000, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	p.Provider, p.MerchantAccount, p.AttemptID, p.ProviderReference, p.Market = "klarna", "K123456", "attempt-1", "order-1", market
	if _, err := p.Apply(tender.Snapshot{Status: tender.PaymentCaptured, Funds: tender.Funds{Authorized: 35000, Captured: 35000, Refunded: 2500}}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, func(tx tender.Tx) error {
		if err := tx.InsertPayment(p); err != nil {
			return err
		}
		for _, op := range []struct {
			id, ref string
			kind    tender.OperationKind
			minor   int64
		}{{"cap-1", "capture-1", tender.OpCapture, 20000}, {"cap-2", "capture-2", tender.OpCapture, 15000}, {"ref-1", "refund-1", tender.OpRefund, 2500}} {
			record := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: op.id, PaymentID: p.ID, AttemptID: p.AttemptID, Key: "key-" + op.id, ProviderKey: "key-" + op.id, ProviderReference: op.ref, Kind: op.kind, Status: tender.OperationSucceeded, Amount: tender.Money{Minor: op.minor, Currency: "USD"}, BaseVersion: 1}}
			if err := tx.InsertOperation(record); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	engine := func(s tender.Store) *tender.Engine {
		e, err := tender.NewEngine(tender.EngineConfig{Store: s, Provider: storageProvider{}, Scope: scope, ProviderName: "klarna", MerchantAccount: "K123456", Market: market, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	detail := func(typ, detailed, amount, capture, refund string) map[string]string {
		return map[string]string{"type": typ, "detailed_type": detailed, "amount": amount, "order_id": "order-1", "capture_id": capture, "refund_id": refund, "payment_reference": "payout-1", "currency_code": "USD", "purchase_country": "US", "merchant_reference1": "cart-1", "Tender Payout Merchant ID": "K123456", "Tender Expected Merchant ID": "K123456", "Tender Payout Reference": "payout-1", "Tender Payout Settlement Type": "NET", "Tender Verified Product": "FIXED_AMOUNT", "Tender Verified Order ID": "order-1", "Tender Source Part ID": "part-1", "Tender Source Section ID": "section-1"}
	}
	rows := []map[string]string{detail("SALE", "PURCHASE", "20000", "capture-1", ""), detail("SALE", "PURCHASE", "15000", "capture-2", ""), detail("RETURN", "PURCHASE_RETURN", "2500", "", "refund-1"), detail("FEE", "PURCHASE_FEE_FIXED", "35", "capture-1", ""), detail("FEE", "PURCHASE_FEE_PERCENTAGE", "600", "capture-2", "")}
	summary := map[string]string{"payment_reference": "payout-1", "merchant_id": "K123456", "currency_code": "USD", "merchant_settlement_type": "NET", "payout_date": "2026-11-25T12:00:00Z", "totals.sale_amount": "35000", "totals.return_amount": "2500", "totals.fee_amount": "635", "totals.settlement_amount": "31865", "Tender Expected Merchant ID": "K123456", "Tender Source Part ID": "part-1", "Tender Source Section ID": "section-1"}
	for _, k := range []string{"commission_amount", "commission_reversal_amount", "repay_amount", "holdback_amount", "tax_amount", "fee_correction_amount", "reversal_amount", "release_amount", "charge_amount", "credit_amount", "fee_refund_amount", "tax_refund_amount", "deposit_amount", "opening_debt_balance_amount", "closing_debt_balance_amount"} {
		summary["totals."+k] = "0"
	}
	declared := int64(31865)
	parts := []tender.ReportPart{{ID: "part-1", SectionID: "section-1", Digest: "host-authenticated-page-digest", Number: 1, TotalParts: 1, DeclaredRows: 6}}
	sections := []tender.ReportSection{{ID: "section-1", BatchID: "payout-1", Currency: "USD", Number: 1, TotalSections: 1, DeclaredRows: 6, DeclaredNetMinor: &declared, DeclaredPayoutMinor: &declared}}
	report, err := klarna.ProjectNETReport("klarna-net-1", summary, rows, parts, sections, now)
	if err != nil {
		t.Fatal(err)
	}
	run, err := engine(store).Reconcile(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Results) != 3 || len(run.PayoutResults) != 1 || run.PayoutResults[0].Outcome != tender.PayoutMatched {
		t.Fatalf("run: %+v", run)
	}
	for _, r := range run.Results {
		if r.Outcome != tender.ReconciliationMatched {
			t.Fatalf("line: %+v", r)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = open()
	replay, err := engine(postgres.New(db)).Reconcile(ctx, report)
	if err != nil || len(replay.PayoutResults) != 1 || replay.PayoutResults[0].Outcome != tender.PayoutMatched {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	var history int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM tender_reconciliation_history WHERE merchant_id=$1 AND run_id=$2`, scope.MerchantID, report.ID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if history != 3 {
		t.Fatalf("replay duplicated history: %d", history)
	}
	duplicate := report
	duplicate.ID = "klarna-duplicate"
	duplicate.Lines = append([]tender.SettlementLine(nil), report.Lines...)
	duplicate.Lines[1].ID = "different-row-id"
	duplicate.Lines[1].ModificationReference = "capture-1"
	duplicate.Lines[1].GrossMinor, duplicate.Lines[1].NetMinor = 20000, 20000
	duplicate.Payouts, duplicate.Adjustments, duplicate.Parts, duplicate.Sections = nil, nil, nil, nil
	dupRun, err := engine(postgres.New(db)).Reconcile(ctx, duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if dupRun.Results[0].Outcome == tender.ReconciliationMatched || dupRun.Results[1].Outcome == tender.ReconciliationMatched {
		t.Fatalf("duplicate capture matched: %+v", dupRun.Results)
	}
	incomplete := report
	incomplete.ID = "klarna-incomplete"
	incomplete.Parts = []tender.ReportPart{{ID: "part-1", SectionID: "section-1", Digest: "page-digest", Number: 1, TotalParts: 2, DeclaredRows: 6}}
	incompleteRun, err := engine(postgres.New(db)).Reconcile(ctx, incomplete)
	if err != nil || incompleteRun.PayoutResults[0].Outcome != tender.PayoutIncomplete {
		t.Fatalf("incomplete: %+v %v", incompleteRun.PayoutResults, err)
	}
}
