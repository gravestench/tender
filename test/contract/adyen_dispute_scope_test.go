//go:build integration

package contract_test

import (
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/store"
	"github.com/gravestench/tender/store/postgres"
)

func TestAdyenDisputePSPCannotBeAnotherPaymentsOperation(t *testing.T) {
	run := func(t *testing.T, db adyenAdverseStore) {
		h := newAdyenRig(t, db, "Authorised", "")
		h.create()
		authA, err := h.confirm()
		if err != nil {
			t.Fatal(err)
		}
		h.process()
		if _, err = h.e.Capture(t.Context(), h.request("capture-a", 100)); err != nil {
			t.Fatal(err)
		}
		h.process()

		createB := tender.Request{PaymentID: "pay-b", OrderID: "order-b", Key: "create-b", Amount: tender.Money{Minor: 100, Currency: "USD"}}
		if _, err = h.e.Create(t.Context(), createB); err != nil {
			t.Fatal(err)
		}
		confirmB := tender.Request{PaymentID: "pay-b", Key: "confirm-b", Amount: createB.Amount, Token: cardInput}
		if _, err = h.e.Confirm(t.Context(), confirmB); err != nil {
			t.Fatal(err)
		}
		h.process()
		captureB, err := h.e.Capture(t.Context(), tender.Request{PaymentID: "pay-b", Key: "capture-b", Amount: createB.Amount})
		if err != nil || captureB.Operation.ProviderReference == "" {
			t.Fatalf("second payment capture = %+v %v", captureB, err)
		}
		h.process()

		body := h.sim.AdyenAdverseNotification(h.record(authA), "CHARGEBACK", captureB.Operation.ProviderReference, 100)
		if status := h.post(body); status != http.StatusAccepted {
			t.Fatalf("signed callback status = %d", status)
		}
		id := h.next()
		if err := h.hook.Process(t.Context(), id); !errors.Is(err, tender.ErrConflict) {
			t.Fatalf("cross-payment operation PSP accepted as dispute: %v", err)
		}
		if notices, err := db.AdverseReviews(t.Context(), dbScope); err != nil || len(notices) != 0 {
			t.Fatalf("cross-payment dispute notice = %+v %v", notices, err)
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
