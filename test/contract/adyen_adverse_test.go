//go:build integration

package contract_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/providers/adyen"
	"github.com/gravestench/tender/store"
	"github.com/gravestench/tender/store/postgres"
)

type adyenAdverseStore interface {
	adyenStore
	AdverseReviews(context.Context, tender.Scope) ([]tender.AdverseNotice, error)
}

func TestAdyenAdverseReportBatch(t *testing.T) {
	if os.Getenv("TENDER_TEST_DATABASE_URL") == "" {
		t.Skip("set TENDER_TEST_DATABASE_URL")
	}
	db := postgres.New(postgresFixture(t)())
	h := newAdyenRig(t, db, "Authorised", "")
	h.create()
	if _, err := h.confirm(); err != nil {
		t.Fatal(err)
	}
	h.process()
	capture, err := h.e.Capture(t.Context(), h.request("capture", 100))
	if err != nil {
		t.Fatal(err)
	}
	h.process()
	refund, err := h.e.Refund(t.Context(), h.request("refund", 25))
	if err != nil {
		t.Fatal(err)
	}
	h.process()
	captureRecord, refundRecord := h.record(capture), h.record(refund)
	base := map[string]string{
		"Merchant Account": testAccount, "Tender Merchant Account": testAccount,
		"Psp Reference": captureRecord.PaymentPSP, "Payment Method": "visa",
		"Gross Currency": "USD", "Net Currency": "USD", "Batch Number": "42",
		"Tender Source Part ID": "part-1", "Tender Source Section ID": "section-42",
	}
	clone := func() map[string]string {
		m := make(map[string]string, len(base))
		for k, v := range base {
			m[k] = v
		}
		return m
	}
	now := time.Now().UTC()
	sale := clone()
	sale["Type"], sale["Merchant Reference"], sale["Modification Reference"] = "Settled", captureRecord.Reference, captureRecord.PSP
	sale["Gross Credit (GC)"], sale["Net Credit (NC)"], sale["Commission (NC)"] = "1.00", "0.97", "0.03"
	saleLine, err := adyen.ProjectCardSettlement(sale, now)
	if err != nil {
		t.Fatal(err)
	}
	refundRow := clone()
	refundRow["Type"], refundRow["Merchant Reference"], refundRow["Modification Reference"] = "Refunded", refundRecord.Reference, refundRecord.PSP
	refundRow["Gross Debit (GC)"], refundRow["Net Debit (NC)"] = "0.25", "0.25"
	refundLine, err := adyen.ProjectCardSettlement(refundRow, now)
	if err != nil {
		t.Fatal(err)
	}
	feeRow := clone()
	feeRow["Type"], feeRow["Modification Reference"] = "Fee", "Transaction Fees May 2026"
	feeRow["Psp Reference"], feeRow["Payment Method"], feeRow["Gross Currency"] = "", "", ""
	feeRow["Net Debit (NC)"] = "0.01"
	fee, err := adyen.ProjectAdjustment(feeRow, now)
	if err != nil {
		t.Fatal(err)
	}
	payoutRow := clone()
	payoutRow["Type"], payoutRow["Modification Reference"] = "MerchantPayout", "TX4313726XT batch 42, TestMerchant"
	payoutRow["Psp Reference"], payoutRow["Payment Method"], payoutRow["Gross Currency"] = "", "", ""
	payoutRow["Net Debit (NC)"] = "0.71"
	payout, err := adyen.ProjectPayout(payoutRow, now)
	if err != nil {
		t.Fatal(err)
	}
	net, amount := int64(71), int64(71)
	report := tender.SettlementReport{ID: "adyen-projected-batch", Feed: tender.FeedHostVerified,
		Lines: []tender.SettlementLine{saleLine, refundLine}, Adjustments: []tender.PayoutAdjustment{fee}, Payouts: []tender.PayoutLine{payout},
		Parts:    []tender.ReportPart{{ID: "part-1", SectionID: "section-42", Digest: "authenticated-artifact", Number: 1, TotalParts: 1, DeclaredRows: 4}},
		Sections: []tender.ReportSection{{ID: "section-42", BatchID: "42", Currency: "USD", Number: 1, TotalSections: 1, DeclaredRows: 4, DeclaredNetMinor: &net, DeclaredPayoutMinor: &amount}},
	}
	run, err := h.e.Reconcile(t.Context(), report)
	if err != nil || len(run.Results) != 2 || run.Results[0].Outcome != tender.ReconciliationMatched || run.Results[1].Outcome != tender.ReconciliationMatched || len(run.PayoutResults) != 1 || run.PayoutResults[0].Outcome != tender.PayoutMatched {
		t.Fatalf("complete batch: %+v %v", run, err)
	}
	incomplete := report
	incomplete.ID = "adyen-incomplete-batch"
	incomplete.Parts = []tender.ReportPart{{ID: "part-1", SectionID: "section-42", Digest: "authenticated-artifact", Number: 1, TotalParts: 2, DeclaredRows: 4}}
	run, err = h.e.Reconcile(t.Context(), incomplete)
	if err != nil || run.PayoutResults[0].Outcome == tender.PayoutMatched {
		t.Fatalf("missing part matched: %+v %v", run.PayoutResults, err)
	}
	otherCreate := h.request("other-create", 100)
	otherCreate.PaymentID, otherCreate.OrderID = "other", "other-order"
	if _, err := h.e.Create(t.Context(), otherCreate); err != nil {
		t.Fatal(err)
	}
	otherPay := h.request("other-pay", 100)
	otherPay.PaymentID, otherPay.Token = "other", cardInput
	if _, err := h.e.Confirm(t.Context(), otherPay); err != nil {
		t.Fatal(err)
	}
	h.process()
	otherCapture := h.request("other-capture", 100)
	otherCapture.PaymentID = "other"
	otherResult, err := h.e.Capture(t.Context(), otherCapture)
	if err != nil {
		t.Fatal(err)
	}
	h.process()
	otherRecord := h.record(otherResult)
	collision := report
	collision.ID = "adyen-cross-payment-modification"
	collision.Lines = append([]tender.SettlementLine(nil), report.Lines...)
	collision.Lines[0].ModificationReference = otherRecord.PSP
	run, err = h.e.Reconcile(t.Context(), collision)
	if err != nil || run.Results[0].Outcome == tender.ReconciliationMatched || run.PayoutResults[0].Outcome == tender.PayoutMatched {
		t.Fatalf("cross-payment modification matched: %+v %v", run, err)
	}
	duplicate := report
	duplicate.ID = "adyen-duplicate-payout"
	duplicate.Payouts = []tender.PayoutLine{payout, payout}
	if _, err := h.e.Reconcile(t.Context(), duplicate); err == nil {
		t.Fatal("duplicate payout row accepted")
	}
}

func TestAdyenAdverseChargeback(t *testing.T) {
	run := func(t *testing.T, db adyenAdverseStore) string {
		h := newAdyenRig(t, db, "Authorised", "")
		h.create()
		auth, err := h.confirm()
		if err != nil {
			t.Fatal(err)
		}
		h.process()
		_, err = h.e.Capture(t.Context(), h.request("capture", 100))
		if err != nil {
			t.Fatal(err)
		}
		h.process()
		before, err := db.Outbox(t.Context(), dbScope)
		if err != nil || len(before) != 1 {
			t.Fatalf("before outbox: %+v %v", before, err)
		}
		authRecord := h.record(auth)
		body := h.sim.AdyenAdverseNotification(authRecord, "CHARGEBACK", "DISPUTE123", 100)
		if status := h.post(body); status != http.StatusAccepted {
			t.Fatalf("callback status %d", status)
		}
		id := h.next()
		if err := h.hook.Process(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		notices, err := db.AdverseReviews(t.Context(), dbScope)
		if err != nil || len(notices) != 1 {
			t.Fatalf("notices: %+v %v", notices, err)
		}
		if n := notices[0]; n.Kind != "chargeback" || n.PaymentID != auth.Payment.ID || n.ProviderEventID != id || n.OriginalReference != authRecord.PaymentPSP || n.ModificationReference != "DISPUTE123" || n.Amount.Minor != 100 || n.OperationID != "" {
			t.Fatalf("notice: %+v", n)
		}
		if err := h.hook.Process(t.Context(), id); err != nil {
			t.Fatalf("replay: %v", err)
		}
		after, err := db.Outbox(t.Context(), dbScope)
		if err != nil || len(after) != len(before) {
			t.Fatalf("changed outbox: %+v %v", after, err)
		}
		var p tender.Payment
		err = db.Transact(t.Context(), func(tx tender.Tx) error { var e error; p, e = tx.Payment(dbScope, auth.Payment.ID); return e })
		if err != nil || p.Funds.Captured != 100 || p.Funds.Returned != 0 {
			t.Fatalf("changed funds: %+v %v", p, err)
		}
		if status := h.post(h.sim.AdyenAdverseNotification(authRecord, "CHARGEBACK", "WRONG", 101)); status != http.StatusAccepted {
			t.Fatalf("wrong amount status %d", status)
		}
		if err := h.hook.Process(t.Context(), h.next()); !errors.Is(err, tender.ErrConflict) {
			t.Fatalf("wrong amount: %v", err)
		}
		if status := h.post(h.sim.AdyenAdverseNotification(mockserver.AdyenRecord{MerchantAccount: testAccount, Reference: authRecord.Reference, PaymentPSP: "OTHERPSP", Currency: "USD"}, "CHARGEBACK", "DISPUTE999", 100)); status != http.StatusAccepted {
			t.Fatalf("wrong original status %d", status)
		}
		if err := h.hook.Process(t.Context(), h.next()); !errors.Is(err, tender.ErrConflict) {
			t.Fatalf("wrong original: %v", err)
		}
		return id
	}
	t.Run("memory", func(t *testing.T) { run(t, store.NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv("TENDER_TEST_DATABASE_URL") == "" {
			t.Skip("set TENDER_TEST_DATABASE_URL")
		}
		open := postgresFixture(t)
		first := open()
		id := run(t, postgres.New(first))
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		restarted := postgres.New(open())
		notices, err := restarted.AdverseReviews(t.Context(), dbScope)
		if err != nil || len(notices) != 1 || notices[0].ProviderEventID != id {
			t.Fatalf("restart lost notice: %+v %v", notices, err)
		}
	})
}

func TestAdyenAdverseRefundFailed(t *testing.T) {
	run := func(t *testing.T, db adyenAdverseStore) {
		h := newAdyenRig(t, db, "Authorised", "")
		h.create()
		if _, err := h.confirm(); err != nil {
			t.Fatal(err)
		}
		h.process()
		if _, err := h.e.Capture(t.Context(), h.request("capture", 100)); err != nil {
			t.Fatal(err)
		}
		h.process()
		refund, err := h.e.Refund(t.Context(), h.request("refund", 25))
		if err != nil {
			t.Fatal(err)
		}
		h.process()
		fx := h.record(refund)
		if status := h.post(h.sim.AdyenAdverseNotification(fx, "REFUND_FAILED", fx.PSP, 25)); status != http.StatusAccepted {
			t.Fatalf("callback status %d", status)
		}
		id := h.next()
		if err := h.hook.Process(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		notices, err := db.AdverseReviews(t.Context(), dbScope)
		if err != nil || len(notices) != 1 {
			t.Fatalf("notices: %+v %v", notices, err)
		}
		if n := notices[0]; n.Kind != "refund_failed" || n.OperationID != refund.Operation.ID || n.ModificationReference != fx.PSP || n.Amount.Minor != 25 {
			t.Fatalf("notice: %+v", n)
		}
		var p tender.Payment
		err = h.db.Transact(t.Context(), func(tx tender.Tx) error { var e error; p, e = tx.Payment(dbScope, refund.Payment.ID); return e })
		if err != nil || p.Funds.Refunded != 25 || p.Funds.Captured != 100 {
			t.Fatalf("history changed: %+v %v", p, err)
		}
		if err := h.hook.Process(t.Context(), id); err != nil {
			t.Fatalf("replay: %v", err)
		}
		if status := h.post(h.sim.AdyenAdverseNotification(fx, "REFUND_FAILED", "UNKNOWNPSP", 25)); status != http.StatusAccepted {
			t.Fatalf("wrong modification status %d", status)
		}
		if err := h.hook.Process(t.Context(), h.next()); !errors.Is(err, tender.ErrNotFound) && !errors.Is(err, tender.ErrConflict) {
			t.Fatalf("wrong modification: %v", err)
		}
	}
	t.Run("memory", func(t *testing.T) { run(t, store.NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv("TENDER_TEST_DATABASE_URL") == "" {
			t.Skip("set TENDER_TEST_DATABASE_URL")
		}
		run(t, postgres.New(postgresFixture(t)()))
	})
}

func TestAdyenAdverseRefundFailedBeforeAcknowledgment(t *testing.T) {
	run := func(t *testing.T, db adyenAdverseStore) {
		h := newAdyenRig(t, db, "Authorised", "")
		h.create()
		if _, err := h.confirm(); err != nil {
			t.Fatal(err)
		}
		h.process()
		if _, err := h.e.Capture(t.Context(), h.request("capture", 100)); err != nil {
			t.Fatal(err)
		}
		h.process()
		if err := h.sim.SetScenarios("adyen.refunds", mockserver.Scenario{Fault: mockserver.LoseResponse}); err != nil {
			t.Fatal(err)
		}
		lost, err := h.e.Refund(t.Context(), h.request("lost-refund", 25))
		if !errors.Is(err, tender.ErrUnknownOutcome) {
			t.Fatalf("lost reply: %v", err)
		}
		positiveID := h.next()
		fx := h.record(lost)
		if status := h.post(h.sim.AdyenAdverseNotification(fx, "REFUND_FAILED", fx.PSP, 25)); status != http.StatusAccepted {
			t.Fatalf("callback status %d", status)
		}
		failureID := h.next()
		if err := h.hook.Process(t.Context(), failureID); !errors.Is(err, tender.ErrConflict) {
			t.Fatalf("unbound failure: %v", err)
		}
		if err := h.hook.Process(t.Context(), positiveID); err == nil {
			t.Fatal("refund was applied despite received failure")
		}
		var payment tender.Payment
		if err := h.db.Transact(t.Context(), func(tx tender.Tx) error { var e error; payment, e = tx.Payment(dbScope, lost.Payment.ID); return e }); err != nil {
			t.Fatal(err)
		}
		if payment.Funds.Refunded != 0 {
			t.Fatalf("unverified refund principal: %+v", payment.Funds)
		}
		restarted := h.engine(h.cfg, nil)
		if result, err := restarted.Recover(t.Context(), lost.Operation.ID); err != nil || result.Operation.ProviderReference != fx.PSP {
			t.Fatalf("recovery: %+v %v", result.Operation, err)
		}
		if err := h.hook.Process(t.Context(), failureID); err != nil {
			t.Fatalf("correlated failure: %v", err)
		}
		if err := h.hook.Process(t.Context(), positiveID); err == nil {
			t.Fatal("positive refund overrode failed refund")
		}
		if err := h.db.Transact(t.Context(), func(tx tender.Tx) error { var e error; payment, e = tx.Payment(dbScope, lost.Payment.ID); return e }); err != nil {
			t.Fatal(err)
		}
		if payment.Funds.Refunded != 0 {
			t.Fatalf("failed refund changed principal: %+v", payment.Funds)
		}
	}
	t.Run("memory", func(t *testing.T) { run(t, store.NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv("TENDER_TEST_DATABASE_URL") == "" {
			t.Skip("set TENDER_TEST_DATABASE_URL")
		}
		run(t, postgres.New(postgresFixture(t)()))
	})
}
