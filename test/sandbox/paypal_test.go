//go:build sandbox

package sandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/providers/paypal"
	"github.com/gravestench/tender/store"
)

const walletSandboxBase = "https://api-m.sandbox.paypal.com"

// This opt-in gate performs authenticated reads and verification postbacks for
// one previously approved real sandbox order. It cannot open a buyer UI or
// initiate money movement. Keep the merchant's evidence file out of the repo.
func TestPayPalSandboxTransaction(t *testing.T) {
	if os.Getenv("PAYMENTS_ENV") != "sandbox" {
		t.Fatal("BLOCKED: select PAYMENTS_ENV=sandbox")
	}
	for _, key := range []string{"TENDER_PAYPAL_CLIENT_ID", "TENDER_PAYPAL_SECRET", "TENDER_PAYPAL_MERCHANT_ACCOUNT", "TENDER_PAYPAL_WEBHOOK_ID", "TENDER_PAYPAL_SANDBOX_PROOF_FILE"} {
		if os.Getenv(key) == "" {
			t.Fatalf("BLOCKED: missing %s", key)
		}
	}
	if err := regularEvidenceFile(os.Getenv("TENDER_PAYPAL_SANDBOX_PROOF_FILE")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(os.Getenv("TENDER_PAYPAL_SANDBOX_PROOF_FILE"))
	if err != nil {
		t.Fatal("BLOCKED: merchant evidence file unavailable")
	}
	defer f.Close()
	var proof struct {
		Payment                            tender.Payment
		Confirm                            tender.Operation
		ExpectedBuyerID, ExpectedCaptureID string
		Headers                            http.Header
		Event                              json.RawMessage
	}
	dec := json.NewDecoder(io.LimitReader(f, 512<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&proof) != nil || proof.ExpectedBuyerID == "" || proof.ExpectedCaptureID == "" || len(proof.Event) == 0 {
		t.Fatal("BLOCKED: provide original payment, confirmation, buyer/capture IDs and real webhook delivery")
	}
	if !validPayPalDelivery(proof.Headers, proof.Event) {
		t.Fatal("BLOCKED: webhook export lacks the original provider delivery headers or capture resource")
	}
	p := proof.Payment
	op := proof.Confirm
	if p.Scope.Validate() != nil || p.Scope.Environment != tender.EnvironmentSandbox || p.Provider != "paypal" || p.MerchantAccount != os.Getenv("TENDER_PAYPAL_MERCHANT_ACCOUNT") || p.Market != (tender.Market{Country: "US", Currency: "USD", Method: tender.MethodPayPal}) || p.ProviderReference == "" || op.Kind != tender.OpConfirm || op.Amount != p.Amount || p.Funds != (tender.Funds{}) || op.ProviderReference != "" || op.BaseVersion != p.Version || op.Status != tender.OperationUnknown || op.ProviderEvidenceDigest == ([32]byte{}) {
		t.Fatal("BLOCKED: expected the original uncollected, merchant-bound sandbox payment and uncertain confirmation with its pre-dispatch buyer digest")
	}
	cfg := paypal.Config{Scope: p.Scope, MerchantAccount: p.MerchantAccount, BaseURL: walletSandboxBase, ClientID: os.Getenv("TENDER_PAYPAL_CLIENT_ID"), Secret: os.Getenv("TENDER_PAYPAL_SECRET"), WebhookID: os.Getenv("TENDER_PAYPAL_WEBHOOK_ID")}
	a, err := paypal.New(cfg)
	if err != nil {
		t.Fatal("invalid sandbox configuration")
	}
	if a.Capabilities().Create || len(a.Capabilities().ActionModes) != 0 {
		t.Fatal("ordinary sandbox web checkout unexpectedly available")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	got, err := a.Query(ctx, tender.ProviderRequest{Payment: p, Operation: op})
	if err != nil || got.Status != tender.OperationSucceeded || got.OperationReference != proof.ExpectedCaptureID || got.Snapshot.VerifiedMethod != tender.MethodPayPal || got.Snapshot.Funds.Captured != p.Amount.Minor {
		t.Fatal("real order did not verify expected capture, seller, wallet, amount and currency")
	}
	if !sandboxWalletBuyer(ctx, cfg, p.ProviderReference, proof.ExpectedBuyerID) {
		t.Fatal("sandbox buyer account mismatch or unavailable")
	}
	db := store.NewMemory()
	err = db.Transact(ctx, func(tx tender.Tx) error {
		if err := tx.InsertPayment(p); err != nil {
			return err
		}
		return tx.InsertOperation(tender.OperationRecord{Operation: op, PaymentVersion: p.Version})
	})
	if err != nil {
		t.Fatal("invalid original host reservation")
	}
	e, err := tender.NewEngine(tender.EngineConfig{Store: db, Provider: a, Scope: p.Scope, Market: p.Market, ProviderName: "paypal", MerchantAccount: p.MerchantAccount})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	hook, err := a.Webhook(db, e, func(event string) { id = event })
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequestWithContext(ctx, "POST", "/paypal/webhook", bytes.NewReader(proof.Event))
		req.Header = proof.Headers.Clone()
		reply := httptest.NewRecorder()
		hook.ServeHTTP(reply, req)
		if reply.Code != 202 || id == "" {
			t.Fatal("real webhook postback verification or durable receipt failed")
		}
		if err = hook.Process(ctx, id); err != nil {
			t.Fatal("real webhook did not match the reserved capture")
		}
	}
	pay, err := e.Get(ctx, p.ID)
	entries, boxErr := db.Outbox(ctx, p.Scope)
	if err != nil || boxErr != nil || len(entries) != 1 || pay.Funds != got.Snapshot.Funds {
		t.Fatal("provider query and verified webhook disagree")
	}
	t.Fatal("BLOCKED: read-only order and webhook postback do not prove host-issued adapter capture, authenticated settlement/payout report, or an approved on-page buyer path")
}

// This is a shape check only. The handler's official PayPal verification
// postback below remains mandatory before the event can be processed.
func validPayPalDelivery(headers http.Header, raw json.RawMessage) bool {
	var transmissionTime string
	for _, key := range []string{"PAYPAL-TRANSMISSION-ID", "PAYPAL-TRANSMISSION-TIME", "PAYPAL-TRANSMISSION-SIG", "PAYPAL-AUTH-ALGO", "PAYPAL-CERT-URL"} {
		var values []string
		for name, entries := range headers {
			if strings.EqualFold(name, key) {
				values = append(values, entries...)
			}
		}
		if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
			return false
		}
		if key == "PAYPAL-TRANSMISSION-TIME" {
			transmissionTime = values[0]
		}
	}
	observed, err := time.Parse(time.RFC3339Nano, transmissionTime)
	if err != nil || !freshProviderTime(observed, time.Now()) {
		return false
	}
	var event struct {
		ID        string `json:"id"`
		EventType string `json:"event_type"`
		Resource  struct {
			ID string `json:"id"`
		} `json:"resource"`
	}
	return json.Unmarshal(raw, &event) == nil && event.ID != "" && event.EventType == "PAYMENT.CAPTURE.COMPLETED" && event.Resource.ID != ""
}

// Query deliberately omits buyer PII from normalized evidence. Read the
// payer account independently and compare in memory without logging either ID.
func sandboxWalletBuyer(ctx context.Context, c paypal.Config, order, buyer string) bool {
	client := &http.Client{Transport: &http.Transport{}, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect") }}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, "POST", walletSandboxBase+"/v1/oauth2/token", strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return false
	}
	req.SetBasicAuth(c.ClientID, c.Secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	var token struct {
		Access string `json:"access_token"`
	}
	err = json.NewDecoder(io.LimitReader(res.Body, 16<<10)).Decode(&token)
	res.Body.Close()
	if err != nil || res.StatusCode != 200 || token.Access == "" {
		return false
	}
	req, err = http.NewRequestWithContext(ctx, "GET", walletSandboxBase+"/v2/checkout/orders/"+order, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token.Access)
	res, err = client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	var o struct {
		ID      string
		Sources struct {
			PayPal struct {
				Account string `json:"account_id"`
			} `json:"paypal"`
		} `json:"payment_source"`
	}
	return res.StatusCode == 200 && json.NewDecoder(io.LimitReader(res.Body, 512<<10)).Decode(&o) == nil && o.ID == order && o.Sources.PayPal.Account == buyer
}

func TestPayPalSandboxRejectsSyntheticCallback(t *testing.T) {
	if validPayPalDelivery(http.Header{"PAYPAL-TRANSMISSION-ID": {"synthetic"}}, json.RawMessage(`{"event_type":"PAYMENT.CAPTURE.COMPLETED"}`)) {
		t.Fatal("accepted an unsigned synthetic callback as provider delivery")
	}
}

func TestPayPalSandboxRejectsStaleSignedDelivery(t *testing.T) {
	h := http.Header{}
	for _, key := range []string{"PAYPAL-TRANSMISSION-ID", "PAYPAL-TRANSMISSION-SIG", "PAYPAL-AUTH-ALGO", "PAYPAL-CERT-URL"} {
		h.Set(key, "value")
	}
	h.Set("PAYPAL-TRANSMISSION-TIME", "2020-01-02T03:04:05Z")
	event := json.RawMessage(`{"id":"evt","event_type":"PAYMENT.CAPTURE.COMPLETED","resource":{"id":"capture"}}`)
	if validPayPalDelivery(h, event) {
		t.Fatal("accepted stale provider-signed transmission time")
	}
}
