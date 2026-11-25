//go:build integration

package contract_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/store"
	"github.com/gravestench/tender/store/postgres"
)

func TestAdyenLaterAdverseDisputeLifecycle(t *testing.T) {
	run := func(t *testing.T, db adyenAdverseStore) {
		h := newAdyenRig(t, db, "Authorised", "")
		h.create()
		auth, err := h.confirm()
		if err != nil {
			t.Fatal(err)
		}
		h.process()
		capture, err := h.e.Capture(t.Context(), h.request("capture", 100))
		if err != nil {
			t.Fatal(err)
		}
		h.process()
		before, err := db.Outbox(t.Context(), dbScope)
		if err != nil || len(before) != 1 {
			t.Fatalf("before outbox: %+v %v", before, err)
		}
		fx := h.record(auth)
		post := func(code string) string {
			t.Helper()
			if status := h.post(h.sim.AdyenAdverseNotification(fx, code, "DISPUTE123", 100)); status != http.StatusAccepted {
				t.Fatalf("%s callback: %d", code, status)
			}
			return h.next()
		}
		reversedID := post("CHARGEBACK_REVERSED")
		secondID := post("SECOND_CHARGEBACK")
		for _, id := range []string{reversedID, secondID} {
			if err := h.hook.Process(t.Context(), id); !errors.Is(err, tender.ErrConflict) {
				t.Fatalf("processed out-of-order %s: %v", id, err)
			}
		}
		chargebackID := post("CHARGEBACK")
		if err := h.hook.Process(t.Context(), chargebackID); err != nil {
			t.Fatal(err)
		}
		if err := h.hook.Process(t.Context(), secondID); !errors.Is(err, tender.ErrConflict) {
			t.Fatalf("second chargeback before reversal: %v", err)
		}
		if err := h.hook.Process(t.Context(), reversedID); err != nil {
			t.Fatalf("process reversal: %v", err)
		}
		changedRef := h.record(capture)
		if changedRef.Reference == fx.Reference {
			t.Fatal("capture and authorization shared merchant reference")
		}
		if status := h.post(h.sim.AdyenAdverseNotification(changedRef, "CHARGEBACK_REVERSED", "DISPUTE123", 100)); status != http.StatusAccepted {
			t.Fatalf("changed reference callback: %d", status)
		}
		if err := h.hook.Process(t.Context(), h.next()); !errors.Is(err, tender.ErrConflict) {
			t.Fatalf("second reversal with same dispute PSP: %v", err)
		}
		for _, id := range []string{secondID, reversedID, secondID} {
			if err := h.hook.Process(t.Context(), id); err != nil {
				t.Fatalf("process/replay %s: %v", id, err)
			}
		}
		notices, err := db.AdverseReviews(t.Context(), dbScope)
		if err != nil || len(notices) != 3 {
			t.Fatalf("notices: %+v %v", notices, err)
		}
		want := map[string]string{chargebackID: "chargeback", reversedID: "chargeback_reversed", secondID: "second_chargeback"}
		for _, n := range notices {
			if n.Kind != want[n.ProviderEventID] || n.PaymentID != auth.Payment.ID || n.OriginalReference != fx.PaymentPSP || n.ModificationReference != "DISPUTE123" || n.Amount.Minor != 100 || n.Amount.Currency != "USD" || n.OperationID != "" {
				t.Fatalf("notice: %+v", n)
			}
			delete(want, n.ProviderEventID)
		}
		if len(want) != 0 {
			t.Fatalf("missing notices: %+v", want)
		}
		for _, tc := range []struct {
			name   string
			record mockserver.AdyenRecord
			psp    string
			amount int64
			status int
		}{
			{"wrong account", func() mockserver.AdyenRecord { x := fx; x.MerchantAccount = "OtherMerchant"; return x }(), "DISPUTE123", 100, http.StatusUnauthorized},
			{"wrong payment PSP", func() mockserver.AdyenRecord { x := fx; x.PaymentPSP = "OTHERPAYMENT"; return x }(), "DISPUTE123", 100, http.StatusAccepted},
			{"missing original PSP", func() mockserver.AdyenRecord { x := fx; x.PaymentPSP = ""; return x }(), "DISPUTE123", 100, http.StatusAccepted},
			{"wrong dispute PSP", fx, "OTHERDISPUTE", 100, http.StatusAccepted},
			{"wrong amount", fx, "DISPUTE123", 99, http.StatusAccepted},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if status := h.post(h.sim.AdyenAdverseNotification(tc.record, "CHARGEBACK_REVERSED", tc.psp, tc.amount)); status != tc.status {
					t.Fatalf("signed callback: %d", status)
				}
				if tc.status != http.StatusAccepted {
					return
				}
				if err := h.hook.Process(t.Context(), h.next()); err == nil {
					t.Fatal("ambiguous reversal was processed")
				}
			})
		}
		notices, err = db.AdverseReviews(t.Context(), dbScope)
		if err != nil || len(notices) != 3 {
			t.Fatalf("ambiguous reversal created notice: %+v %v", notices, err)
		}
		var p tender.Payment
		if err := db.Transact(t.Context(), func(tx tender.Tx) error { var e error; p, e = tx.Payment(dbScope, auth.Payment.ID); return e }); err != nil {
			t.Fatal(err)
		}
		after, err := db.Outbox(t.Context(), dbScope)
		if err != nil || len(after) != len(before) || p.Funds.Captured != 100 || p.Funds.Returned != 0 || p.Funds.Disputed != 0 {
			t.Fatalf("notice changed funds/outbox: %+v %+v %v", p.Funds, after, err)
		}
	}
	t.Run("memory", func(t *testing.T) { run(t, store.NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv("TENDER_TEST_DATABASE_URL") == "" {
			t.Skip("set TENDER_TEST_DATABASE_URL")
		}
		open := postgresFixture(t)
		db := open()
		run(t, postgres.New(db))
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		notices, err := postgres.New(open()).AdverseReviews(context.Background(), dbScope)
		if err != nil || len(notices) != 3 {
			t.Fatalf("restart lost notices: %+v %v", notices, err)
		}
	})
}

func TestAdyenLaterAdverseRefundReversal(t *testing.T) {
	run := func(t *testing.T, db adyenAdverseStore) string {
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
		first, err := h.e.Refund(t.Context(), h.request("refund-one", 25))
		if err != nil {
			t.Fatal(err)
		}
		positiveID := h.next()
		fx := h.record(first)
		if status := h.post(h.sim.AdyenAdverseNotification(fx, "REFUNDED_REVERSED", fx.PSP, 25)); status != http.StatusAccepted {
			t.Fatalf("reversal callback: %d", status)
		}
		reversedID := h.next()
		if err := h.hook.Process(t.Context(), reversedID); !errors.Is(err, tender.ErrConflict) {
			t.Fatalf("reversal before succeeded refund: %v", err)
		}
		if err := h.hook.Process(t.Context(), positiveID); err != nil {
			t.Fatal(err)
		}
		second, err := h.e.Refund(t.Context(), h.request("refund-two", 25))
		if err != nil {
			t.Fatal(err)
		}
		h.process()
		if h.record(second).PSP == fx.PSP {
			t.Fatal("equal partial refunds shared modification PSP")
		}
		before, err := db.Outbox(t.Context(), dbScope)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.hook.Process(t.Context(), reversedID); err != nil {
			t.Fatalf("reversal after refund: %v", err)
		}
		if err := h.hook.Process(t.Context(), reversedID); err != nil {
			t.Fatalf("replayed reversal: %v", err)
		}
		notices, err := db.AdverseReviews(t.Context(), dbScope)
		if err != nil || len(notices) != 1 {
			t.Fatalf("notices: %+v %v", notices, err)
		}
		if n := notices[0]; n.Kind != "refunded_reversed" || n.OperationID != first.Operation.ID || n.ModificationReference != fx.PSP || n.OriginalReference != fx.PaymentPSP || n.Amount.Minor != 25 || n.ProviderEventID != reversedID {
			t.Fatalf("reversal notice: %+v", n)
		}
		for _, tc := range []struct {
			name, psp string
			amount    int64
		}{{"unknown PSP", "OTHERREFUND", 25}, {"wrong amount", fx.PSP, 26}} {
			t.Run(tc.name, func(t *testing.T) {
				if status := h.post(h.sim.AdyenAdverseNotification(fx, "REFUNDED_REVERSED", tc.psp, tc.amount)); status != http.StatusAccepted {
					t.Fatalf("callback: %d", status)
				}
				if err := h.hook.Process(t.Context(), h.next()); !errors.Is(err, tender.ErrConflict) && !errors.Is(err, tender.ErrNotFound) {
					t.Fatalf("ambiguous refund reversal: %v", err)
				}
			})
		}
		p, err := h.e.Get(t.Context(), first.Payment.ID)
		after, outErr := db.Outbox(t.Context(), dbScope)
		if err != nil || outErr != nil || p.Funds.Captured != 100 || p.Funds.Refunded != 50 || p.Funds.Returned != 0 || len(after) != len(before) {
			t.Fatalf("reversal changed funds/outbox: %+v %+v %v %v", p.Funds, after, err, outErr)
		}
		return reversedID
	}
	t.Run("memory", func(t *testing.T) { run(t, store.NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv("TENDER_TEST_DATABASE_URL") == "" {
			t.Skip("set TENDER_TEST_DATABASE_URL")
		}
		open := postgresFixture(t)
		db := open()
		id := run(t, postgres.New(db))
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		notices, err := postgres.New(open()).AdverseReviews(t.Context(), dbScope)
		if err != nil || len(notices) != 1 || notices[0].ProviderEventID != id {
			t.Fatalf("restart lost reversal: %+v %v", notices, err)
		}
	})
}
