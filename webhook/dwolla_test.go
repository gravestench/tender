package webhook_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/providers/dwolla"
	"github.com/gravestench/tender/store"
	"github.com/gravestench/tender/webhook"
)

const (
	dwollaAccount  = "11111111-1111-4111-8111-111111111111"
	dwollaTransfer = "55555555-5555-4555-8555-555555555555"
	dwollaEvent    = "88888888-8888-4888-8888-888888888888"
	dwollaSecret   = "dwolla-test-subscription-secret-32bytes"
)

func TestDwollaWebhookVerifiesAndDurablyDeduplicates(t *testing.T) {
	storage := store.NewMemory()
	scope := tender.Scope{MerchantID: "shop", Environment: tender.EnvironmentMock}
	base := "http://127.0.0.1:8080"
	var wakes int
	h, err := webhook.NewDwolla(webhook.DwollaConfig{Store: storage, Scope: scope, BaseURL: base, MerchantAccount: dwollaAccount, HMACSecrets: []string{dwollaSecret}, OnStored: func(string) { wakes++ }})
	if err != nil {
		t.Fatal(err)
	}
	makeBody := func(resource string) []byte {
		b, _ := json.Marshal(map[string]any{"id": dwollaEvent, "resourceId": resource, "topic": "customer_transfer_created", "timestamp": "2026-11-25T10:00:00Z", "_links": map[string]any{"account": map[string]string{"href": base + "/accounts/" + dwollaAccount}, "resource": map[string]string{"href": base + "/transfers/" + resource}}})
		return b
	}
	post := func(body []byte, valid bool) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		mac := hmac.New(sha256.New, []byte(dwollaSecret))
		mac.Write(body)
		sig := hex.EncodeToString(mac.Sum(nil))
		if !valid {
			sig = "00" + sig[2:]
		}
		r.Header.Set("X-Request-Signature-SHA-256", sig)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	body := makeBody(dwollaTransfer)
	if code := post(body, false); code != 401 {
		t.Fatalf("bad HMAC returned %d", code)
	}
	if code := post(body, true); code != 202 {
		t.Fatalf("signed receipt returned %d", code)
	}
	if code := post(body, true); code != 202 {
		t.Fatalf("same signed receipt returned %d", code)
	}
	if wakes != 1 {
		t.Fatalf("duplicate scheduled %d workers", wakes)
	}
	if code := post(makeBody("99999999-9999-4999-8999-999999999999"), true); code != 409 {
		t.Fatalf("changed event ID accepted: %d", code)
	}
	// Dwolla subscriptions send all topics. An unrelated, signed customer
	// event must be acknowledged without retrying or entering a payment inbox.
	unrelated, _ := json.Marshal(map[string]any{"id": "99999999-9999-4999-8999-999999999999", "resourceId": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "topic": "customer_created", "timestamp": "2026-11-25T10:01:00Z", "_links": map[string]any{"account": map[string]string{"href": base + "/accounts/" + dwollaAccount}, "resource": map[string]string{"href": base + "/customers/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}}})
	if code := post(unrelated, true); code != 202 || wakes != 1 {
		t.Fatalf("unrelated signed event retried or scheduled: code=%d wakes=%d", code, wakes)
	}
	// Topic names can evolve. A signed event about this transfer remains
	// pending evidence even if Tender does not recognize its topic yet.
	unknownID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	unknown, _ := json.Marshal(map[string]any{"id": unknownID, "resourceId": dwollaTransfer, "topic": "ach_returned", "timestamp": "2026-11-25T10:02:00Z", "_links": map[string]any{"account": map[string]string{"href": base + "/accounts/" + dwollaAccount}, "resource": map[string]string{"href": base + "/transfers/" + dwollaTransfer}}})
	if code := post(unknown, true); code != 202 || wakes != 2 {
		t.Fatalf("unknown transfer event was silently dropped: code=%d wakes=%d", code, wakes)
	}
	key := tender.VerifiedEventKey{Scope: scope, Provider: "dwolla", MerchantAccount: dwollaAccount, ID: dwollaEvent}
	var processed int
	if err := storage.Transact(t.Context(), func(tx tender.Tx) error {
		return tx.(tender.VerifiedEventTx).ProcessVerifiedEvent(key, time.Now(), func(raw json.RawMessage) error {
			processed++
			var event struct{ ID, ResourceID, Topic string }
			if json.Unmarshal(raw, &event) != nil || event.ID != dwollaEvent || event.ResourceID != dwollaTransfer || event.Topic != "customer_transfer_created" {
				t.Fatalf("stored event was not allowlisted: %s", raw)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.Transact(context.Background(), func(tx tender.Tx) error {
		return tx.(tender.VerifiedEventTx).ProcessVerifiedEvent(key, time.Now(), func(json.RawMessage) error { processed++; return nil })
	}); err != nil || processed != 1 {
		t.Fatalf("replay processed %d times: %v", processed, err)
	}
}

func TestDwollaSignedFailureCreatesReviewWithoutCollectingFunds(t *testing.T) {
	storage := store.NewMemory()
	scope := tender.Scope{MerchantID: "shop", Environment: tender.EnvironmentMock}
	market := tender.Market{Country: "US", Currency: "USD", Method: tender.MethodACH}
	amount := tender.Money{Currency: "USD", Minor: 1234}
	const operationID = "safe_operation_01"
	base := "http://127.0.0.1:8080"
	adapter, err := dwolla.New(dwolla.Config{Environment: tender.EnvironmentMock, BaseURL: base, MerchantAccount: dwollaAccount, DestinationFundingSourceID: "22222222-2222-4222-8222-222222222222",
		BearerToken: func(context.Context) (string, error) { return "token", nil }, AuthorizeSource: func(context.Context, tender.Payment, dwolla.Source) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := tender.NewEngine(tender.EngineConfig{Store: storage, Provider: adapter, Scope: scope, ProviderName: "dwolla", MerchantAccount: dwollaAccount, Market: market})
	if err != nil {
		t.Fatal(err)
	}
	payment := tender.Payment{Scope: scope, ID: "payment", OrderID: "order", AttemptID: "attempt", Provider: "dwolla", MerchantAccount: dwollaAccount, ProviderReference: dwollaTransfer, Market: market, Amount: amount, Snapshot: tender.Snapshot{Status: tender.PaymentPending}}
	op := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: operationID, PaymentID: payment.ID, AttemptID: payment.AttemptID, Key: "host-key", ProviderKey: "provider-key", ProviderReference: dwollaTransfer, Kind: tender.OpCreate, Status: tender.OperationSucceeded, Amount: amount}}
	if err := storage.Transact(t.Context(), func(tx tender.Tx) error {
		if err := tx.InsertPayment(payment); err != nil {
			return err
		}
		return tx.InsertOperation(op)
	}); err != nil {
		t.Fatal(err)
	}
	h, err := webhook.NewDwolla(webhook.DwollaConfig{Store: storage, Engine: engine, Scope: scope, BaseURL: base, MerchantAccount: dwollaAccount, HMACSecrets: []string{dwollaSecret}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"id": dwollaEvent, "resourceId": dwollaTransfer, "correlationId": operationID, "topic": "customer_transfer_failed", "created": "2026-11-25T10:00:00Z", "_links": map[string]any{"account": map[string]string{"href": base + "/accounts/" + dwollaAccount}, "resource": map[string]string{"href": base + "/transfers/" + dwollaTransfer}}})
	mac := hmac.New(sha256.New, []byte(dwollaSecret))
	mac.Write(body)
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	r.Header.Set("X-Request-Signature-SHA-256", hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("official created field rejected: %d", w.Code)
	}
	if err := h.Process(t.Context(), "wrong-payment", dwollaEvent); err == nil {
		t.Fatal("wrong payment consumed failure")
	}
	if err := h.Process(t.Context(), payment.ID, dwollaEvent); err != nil {
		t.Fatalf("signed failure not projected: %v", err)
	}
	if err := h.Process(t.Context(), payment.ID, dwollaEvent); err != nil {
		t.Fatalf("duplicate failure not idempotent: %v", err)
	}
	badID := "99999999-9999-4999-8999-999999999999"
	badBody, _ := json.Marshal(map[string]any{"id": badID, "resourceId": dwollaTransfer, "correlationId": "other_operation", "topic": "customer_transfer_failed", "created": "2026-11-25T10:01:00Z", "_links": map[string]any{"account": map[string]string{"href": base + "/accounts/" + dwollaAccount}, "resource": map[string]string{"href": base + "/transfers/" + dwollaTransfer}}})
	mac = hmac.New(sha256.New, []byte(dwollaSecret))
	mac.Write(badBody)
	r = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(badBody))
	r.Header.Set("X-Request-Signature-SHA-256", hex.EncodeToString(mac.Sum(nil)))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("mismatched signed event not retained for review: %d", w.Code)
	}
	if err := h.Process(t.Context(), payment.ID, badID); err == nil {
		t.Fatal("mismatched signed correlation created a return review")
	}
	if err := storage.Transact(t.Context(), func(tx tender.Tx) error {
		notices, err := tx.(tender.AdverseEvidenceReader).AdverseEvidence(scope, "dwolla", dwollaAccount, payment.ID)
		if err != nil {
			return err
		}
		if len(notices) != 1 || notices[0].Kind != "ach_return" || notices[0].OriginalReference != dwollaTransfer || notices[0].Amount != amount {
			t.Fatalf("unexpected review evidence: %+v", notices)
		}
		got, err := tx.Payment(scope, payment.ID)
		if err != nil {
			return err
		}
		if got.Status != tender.PaymentPending || got.Funds != (tender.Funds{}) {
			t.Fatalf("failure changed money state: %+v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
