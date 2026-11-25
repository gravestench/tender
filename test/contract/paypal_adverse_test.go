//go:build integration

package contract_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/store"
	"github.com/gravestench/tender/store/postgres"
)

func newAdverseWallet(t *testing.T) *walletRig {
	t.Helper()
	if dsn := os.Getenv("TENDER_TEST_DATABASE_URL"); dsn != "" {
		if !adyenLocalDatabase(dsn) {
			t.Fatal("local-only SQL required")
		}
		return newWallet(t, postgres.New(postgresFixture(t)()))
	}
	return newWallet(t, store.NewMemory())
}

func paypalFixturePost(t *testing.T, h *walletRig, path string) {
	t.Helper()
	res, err := http.Post(h.cfg.BaseURL+path, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 204 {
		t.Fatalf("fixture %s status %d", path, res.StatusCode)
	}
}

func paypalSignedCapture(t *testing.T, h *walletRig, id, kind, order, capture, merchant, value string) int {
	t.Helper()
	status := strings.TrimPrefix(kind, "PAYMENT.CAPTURE.")
	if kind == "PAYMENT.CAPTURE.DENIED" {
		status = "DECLINED"
	}
	raw, err := json.Marshal(map[string]any{
		"id": id, "event_type": kind, "resource_type": "capture",
		"resource": map[string]any{"id": capture, "status": status,
			"amount":             map[string]string{"currency_code": "USD", "value": value},
			"payee":              map[string]string{"merchant_id": merchant},
			"supplementary_data": map[string]any{"related_ids": map[string]string{"order_id": order}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	transmission := "TX-" + id
	mac := hmac.New(sha256.New, []byte("only-a-fixture-key"))
	mac.Write([]byte(transmission + "|" + stamp + "|WH-FIXTURE|"))
	mac.Write(raw)
	req := httptest.NewRequest(http.MethodPost, "/hooks", bytes.NewReader(raw))
	req.Header.Set("PayPal-Auth-Algo", "SHA256withRSA")
	req.Header.Set("PayPal-Cert-Url", h.cfg.BaseURL+"/v1/notifications/certs/CERT-MOCK")
	req.Header.Set("PayPal-Transmission-Id", transmission)
	req.Header.Set("PayPal-Transmission-Time", stamp)
	req.Header.Set("PayPal-Transmission-Sig", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	h.hook.ServeHTTP(w, req)
	return w.Code
}

func TestPayPalAdversePendingThenCompleted(t *testing.T) {
	h := newAdverseWallet(t)
	created := h.create(t)
	h.decide(t, created.Payment.ProviderReference, "approve")
	paypalFixturePost(t, h, "/fixture/capture-next/PENDING")
	result, err := h.engine.Confirm(t.Context(), walletReq("confirm-pending", 1000))
	if err != nil || result.Payment.Funds.Captured != 0 {
		t.Fatalf("pending confirm = %+v %v", result.Payment.Funds, err)
	}
	id := h.event(t)
	if err := h.hook.Process(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if p, _ := h.engine.Get(t.Context(), "wallet-payment"); p.Funds.Captured != 0 {
		t.Fatal("pending collected")
	}
	if box, _ := h.db.Outbox(t.Context(), dbScope); len(box) != 0 {
		t.Fatal("pending fulfilled")
	}
	paypalFixturePost(t, h, "/fixture/orders/"+created.Payment.ProviderReference+"/capture-status/COMPLETED")
	id = h.event(t)
	if err := h.hook.Process(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if p, _ := h.engine.Get(t.Context(), "wallet-payment"); p.Funds.Captured != 1000 {
		t.Fatalf("completed funds = %+v", p.Funds)
	}
}

func TestPayPalAdverseDeniedAndLateReplay(t *testing.T) {
	t.Run("denied before collection", func(t *testing.T) {
		h := newAdverseWallet(t)
		created := h.create(t)
		h.decide(t, created.Payment.ProviderReference, "approve")
		paypalFixturePost(t, h, "/fixture/capture-next/DENIED")
		_, _ = h.engine.Confirm(t.Context(), walletReq("confirm-denied", 1000))
		id := h.event(t)
		if err := h.hook.Process(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if p, _ := h.engine.Get(t.Context(), "wallet-payment"); p.Funds.Captured != 0 {
			t.Fatal("denied collected")
		}
		if box, _ := h.db.Outbox(t.Context(), dbScope); len(box) != 0 {
			t.Fatal("denied fulfilled")
		}
	})
	t.Run("late denial queues one review", func(t *testing.T) {
		h := newAdverseWallet(t)
		created := h.create(t)
		h.decide(t, created.Payment.ProviderReference, "approve")
		_, err := h.engine.Confirm(t.Context(), walletReq("confirm-complete", 1000))
		if err != nil {
			t.Fatal(err)
		}
		if err := h.hook.Process(t.Context(), h.event(t)); err != nil {
			t.Fatal(err)
		}
		paypalFixturePost(t, h, "/fixture/orders/"+created.Payment.ProviderReference+"/capture-status/DENIED")
		id := h.event(t)
		if err := h.hook.Process(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if err := h.hook.Process(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		p, err := h.engine.Get(t.Context(), "wallet-payment")
		if err != nil || p.Funds.Captured != 1000 {
			t.Fatalf("late denial erased capture: %+v %v", p.Funds, err)
		}
		reviews, err := h.db.(interface {
			AdverseReviews(context.Context, tender.Scope) ([]tender.AdverseNotice, error)
		}).AdverseReviews(t.Context(), dbScope)
		if err != nil || len(reviews) != 1 || reviews[0].Kind != "PAYMENT.CAPTURE.DENIED" || reviews[0].OriginalReference != created.Payment.ProviderReference {
			t.Fatalf("reviews = %+v %v", reviews, err)
		}
		if box, _ := h.db.Outbox(t.Context(), dbScope); len(box) != 1 {
			t.Fatal("fulfillment erased or duplicated")
		}
	})
}

func TestPayPalAdverseRejectsWrongCorrelationAndStalePending(t *testing.T) {
	h := newAdverseWallet(t)
	created := h.create(t)
	h.decide(t, created.Payment.ProviderReference, "approve")
	confirmed, err := h.engine.Confirm(t.Context(), walletReq("confirm", 1000))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hook.Process(t.Context(), h.event(t)); err != nil {
		t.Fatal(err)
	}
	cap := confirmed.Operation.ProviderReference
	if got := paypalSignedCapture(t, h, "WH-WRONG-MERCHANT", "PAYMENT.CAPTURE.DENIED", created.Payment.ProviderReference, cap, "OTHER", "10.00"); got == 202 {
		t.Fatal("wrong merchant accepted")
	}
	if got := paypalSignedCapture(t, h, "WH-WRONG-ORDER", "PAYMENT.CAPTURE.DENIED", "OOTHER", cap, h.cfg.MerchantAccount, "10.00"); got != 202 {
		t.Fatalf("wrong order receipt %d", got)
	}
	if err := h.hook.Process(t.Context(), "WH-WRONG-ORDER"); err == nil {
		t.Fatal("wrong order processed")
	}
	if got := paypalSignedCapture(t, h, "WH-WRONG-AMOUNT", "PAYMENT.CAPTURE.DENIED", created.Payment.ProviderReference, cap, h.cfg.MerchantAccount, "99.00"); got != 202 {
		t.Fatalf("wrong amount receipt %d", got)
	}
	if err := h.hook.Process(t.Context(), "WH-WRONG-AMOUNT"); err == nil {
		t.Fatal("wrong amount processed")
	}
	if got := paypalSignedCapture(t, h, "WH-STALE-PENDING", "PAYMENT.CAPTURE.PENDING", created.Payment.ProviderReference, cap, h.cfg.MerchantAccount, "10.00"); got != 202 {
		t.Fatalf("pending receipt %d", got)
	}
	if err := h.hook.Process(t.Context(), "WH-STALE-PENDING"); err == nil {
		t.Fatal("stale pending changed captured payment")
	}
	if p, _ := h.engine.Get(t.Context(), "wallet-payment"); p.Funds.Captured != 1000 {
		t.Fatal("wrong or stale event changed captured funds")
	}
	if reviews, err := h.db.(interface {
		AdverseReviews(context.Context, tender.Scope) ([]tender.AdverseNotice, error)
	}).AdverseReviews(t.Context(), dbScope); err != nil || len(reviews) != 0 {
		t.Fatalf("wrong notice = %+v %v", reviews, err)
	}
}

func TestPayPalAdversePendingCannotReopenDeniedCapture(t *testing.T) {
	h := newAdverseWallet(t)
	created := h.create(t)
	h.decide(t, created.Payment.ProviderReference, "approve")
	paypalFixturePost(t, h, "/fixture/capture-next/DENIED")
	_, _ = h.engine.Confirm(t.Context(), walletReq("confirm-denied", 1000))
	if err := h.hook.Process(t.Context(), h.event(t)); err != nil {
		t.Fatal(err)
	}
	paypalFixturePost(t, h, "/fixture/orders/"+created.Payment.ProviderReference+"/capture-status/PENDING")
	id := h.event(t)
	if err := h.hook.Process(t.Context(), id); err == nil {
		t.Fatal("pending reopened denied capture")
	}
	if p, _ := h.engine.Get(t.Context(), "wallet-payment"); p.Status != tender.PaymentDeclined || p.Funds.Captured != 0 {
		t.Fatalf("denial changed: %+v", p.Snapshot)
	}
}

func TestPayPalAdverseDocumentedDeclinedResource(t *testing.T) {
	h := newAdverseWallet(t)
	created := h.create(t)
	h.decide(t, created.Payment.ProviderReference, "approve")
	paypalFixturePost(t, h, "/fixture/capture-next/DENIED")
	_, _ = h.engine.Confirm(t.Context(), walletReq("confirm-declined", 1000))
	var d walletDelivery
	select {
	case d = <-h.deliveries:
	case <-time.After(4 * time.Second):
		t.Fatal("denied fixture delivery missing")
	}
	var event struct {
		ID       string
		Resource struct{ Status string }
	}
	if err := json.Unmarshal(d.body, &event); err != nil {
		t.Fatal(err)
	}
	if event.Resource.Status != "DECLINED" {
		t.Fatalf("fixture resource status = %q", event.Resource.Status)
	}
	req := httptest.NewRequest(http.MethodPost, "/hooks", bytes.NewReader(d.body))
	req.Header = d.header.Clone()
	w := httptest.NewRecorder()
	h.hook.ServeHTTP(w, req)
	if w.Code != 202 {
		t.Fatalf("documented denial receipt status %d", w.Code)
	}
	if err := h.hook.Process(t.Context(), event.ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := h.engine.Get(t.Context(), "wallet-payment"); p.Funds.Captured != 0 {
		t.Fatal("declined capture collected")
	}
}
