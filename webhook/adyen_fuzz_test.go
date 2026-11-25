package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gravestench/tender"
)

// The public HTTP entry point must reject an oversized body before any inbox
// or engine access, including a syntactically valid JSON prefix.
func TestAdyenWebhookBodyLimit(t *testing.T) {
	h := &Adyen{}
	r := httptest.NewRequest(http.MethodPost, "/webhooks/adyen", bytes.NewReader(bytes.Repeat([]byte(" "), (512<<10)+1)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized webhook HTTP %d; want 413", w.Code)
	}
}

// Valid Standard HMAC evidence must survive wire decoding; arbitrary mutation
// may be rejected but must not cross the account/signature boundary.
func FuzzAdyenStandardNotification(f *testing.F) {
	key := bytes.Repeat([]byte{0x42}, 32)
	amount := int64(1234)
	e := evidence{PSP: "psp-1", Account: "merchant", Ref: "order.aaaaaaaaaaaaaaaaaaaaaa", Code: "AUTHORISATION", Success: "true"}
	e.Amount.Value, e.Amount.Currency = &amount, "USD"
	canonical, _ := e.canonical()
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(canonical))
	body, _ := json.Marshal(map[string]any{"live": "false", "notificationItems": []any{map[string]any{"NotificationRequestItem": map[string]any{
		"pspReference": e.PSP, "originalReference": e.Original, "merchantAccountCode": e.Account, "merchantReference": e.Ref,
		"amount": map[string]any{"value": amount, "currency": "USD"}, "eventCode": e.Code, "success": e.Success,
		"additionalData": map[string]string{"hmacSignature": base64.StdEncoding.EncodeToString(mac.Sum(nil))},
	}}}})
	f.Add(body)
	f.Add([]byte(`{"live":"false","notificationItems":[{"NotificationRequestItem":{"merchantAccountCode":"merchant","eventCode":"AUTHORISATION"}}]}`))
	f.Add([]byte(`{"live":"true","notificationItems":[]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 512<<10 {
			return
		}
		h := &Adyen{cfg: AdyenConfig{Scope: tender.Scope{MerchantID: "merchant", Environment: tender.EnvironmentMock}, MerchantAccount: "merchant"}, keys: [][]byte{key}}
		events, err := h.verified(raw)
		if err != nil {
			if bytes.Equal(raw, body) {
				t.Fatalf("rejected a valid signed Standard notification: %v", err)
			}
			return
		}
		if len(events) == 0 || len(events) > 32 {
			t.Fatalf("accepted invalid notification count %d", len(events))
		}
		for _, got := range events {
			if got.Account != "merchant" {
				t.Fatalf("cross-account webhook accepted: %q", got.Account)
			}
			if _, ok := got.canonical(); !ok {
				t.Fatalf("noncanonical signed webhook accepted: %+v", got)
			}
		}
	})
}
