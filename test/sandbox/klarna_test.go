//go:build sandbox

package sandbox_test

import (
	"encoding/json"
	"fmt"
	"github.com/gravestench/tender/providers/klarna"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

type klarnaHostEvidence struct {
	MerchantAccount, OrderID, MerchantReference1, MerchantReference2, CaptureID, RefundID, FinancingMethod string
	OrderAmount, CapturedAmount, RefundedAmount                                                            int64
}

var klarnaUUID = regexp.MustCompile(`(?i)^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func validKlarnaHost(host klarnaHostEvidence, merchant string) bool {
	return merchant != "" && host.MerchantAccount == merchant && klarnaUUID.MatchString(host.OrderID) && klarnaUUID.MatchString(host.CaptureID) && klarnaUUID.MatchString(host.RefundID) && klarna.IsUSFinancing("US", "USD", host.FinancingMethod) && host.MerchantReference1 != "" && host.MerchantReference2 != "" && host.OrderAmount > 0 && host.CapturedAmount > 0 && host.CapturedAmount <= host.OrderAmount && host.RefundedAmount > 0 && host.RefundedAmount < host.CapturedAmount
}

func validKlarnaCapture(id string, amount int64, capturedAt, expectedID string, expectedAmount int64, now time.Time) bool {
	when, err := time.Parse(time.RFC3339Nano, capturedAt)
	return err == nil && id == expectedID && amount == expectedAmount && freshProviderTime(when, now)
}

// TestKlarnaSandboxOrderRecord compares a previously purchased, partially
// refunded order in the NA playground with independently exported host values.
// It only reads. A pass establishes order-record agreement for this sample.
func TestKlarnaSandboxOrderRecord(t *testing.T) {
	if os.Getenv("PAYMENTS_ENV") != "sandbox" {
		t.Fatal("BLOCKED: select PAYMENTS_ENV=sandbox and supply the merchant's NA playground order evidence")
	}
	key, path, merchant := os.Getenv("TENDER_KLARNA_API_KEY"), os.Getenv("TENDER_KLARNA_HOST_EVIDENCE"), os.Getenv("TENDER_KLARNA_MERCHANT_ACCOUNT")
	if !strings.HasPrefix(key, "klarna_test_api_") || path == "" || merchant == "" {
		t.Fatal("BLOCKED: supply the Klarna NA playground API key, merchant account and host export")
	}
	if err := regularEvidenceFile(path); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal("BLOCKED: host evidence is unreadable")
	}
	defer f.Close()
	var host klarnaHostEvidence
	raw, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(raw) > 8192 || json.Unmarshal(raw, &host) != nil {
		t.Fatal("BLOCKED: invalid host evidence")
	}
	if !validKlarnaHost(host, merchant) {
		t.Fatal("BLOCKED: host evidence must identify a captured US/USD order and its partial refund")
	}
	req, err := http.NewRequestWithContext(t.Context(), "GET", klarna.PlaygroundURL+"/ordermanagement/v1/orders/"+host.OrderID, nil)
	if err != nil {
		t.Fatal("invalid request")
	}
	req.Header.Set("Authorization", "Basic "+key)
	transport := &http.Transport{ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	client := http.Client{Timeout: 15 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal("BLOCKED: NA playground did not return an order")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(fmt.Sprintf("BLOCKED: NA playground order returned HTTP %d", res.StatusCode))
	}
	var provider struct {
		ID      string `json:"order_id"`
		Ref1    string `json:"merchant_reference1"`
		Ref2    string `json:"merchant_reference2"`
		Country string `json:"purchase_country"`
		Initial struct {
			Type string `json:"type"`
		} `json:"initial_payment_method"`
		Currency string `json:"purchase_currency"`
		Original int64  `json:"original_order_amount"`
		Captured int64  `json:"captured_amount"`
		Refunded int64  `json:"refunded_amount"`
		Fraud    string `json:"fraud_status"`
		Captures []struct {
			ID string `json:"capture_id"`
		} `json:"captures"`
		Refunds []struct {
			ID string `json:"refund_id"`
		} `json:"refunds"`
	}
	raw, err = io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	if err != nil || len(raw) > 1<<20 || json.Unmarshal(raw, &provider) != nil {
		t.Fatal("invalid playground order details")
	}
	if provider.ID != host.OrderID || provider.Ref1 != host.MerchantReference1 || provider.Ref2 != host.MerchantReference2 || !klarna.IsUSFinancing(provider.Country, provider.Currency, provider.Initial.Type) || !strings.EqualFold(host.FinancingMethod, provider.Initial.Type) || provider.Fraud != "ACCEPTED" || provider.Original != host.OrderAmount || provider.Captured != host.CapturedAmount || provider.Refunded != host.RefundedAmount {
		t.Fatal("host and playground order identities, route, or amounts differ")
	}
	capture, refund := false, false
	for _, c := range provider.Captures {
		capture = capture || c.ID == host.CaptureID
	}
	for _, r := range provider.Refunds {
		refund = refund || r.ID == host.RefundID
	}
	if !capture || !refund {
		t.Fatal("capture or refund reference missing from playground order")
	}
	// The order record is read through merchant authentication. Check the
	// separately authenticated capture's provider timestamp; CI file mtime is
	// fresh whenever a base64 export is decoded and proves nothing.
	request, err := http.NewRequestWithContext(t.Context(), "GET", klarna.PlaygroundURL+"/ordermanagement/v1/orders/"+host.OrderID+"/captures/"+host.CaptureID, nil)
	if err != nil {
		t.Fatal("invalid capture request")
	}
	request.Header.Set("Authorization", "Basic "+key)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("BLOCKED: NA playground capture details unavailable")
	}
	defer response.Body.Close()
	var captured struct {
		ID     string `json:"capture_id"`
		Amount int64  `json:"captured_amount"`
		At     string `json:"captured_at"`
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&captured) != nil || !validKlarnaCapture(captured.ID, captured.Amount, captured.At, host.CaptureID, host.CapturedAmount, time.Now()) {
		t.Fatal("BLOCKED: authenticated capture is mismatched or stale")
	}
	t.Fatal("BLOCKED: read-only order record does not prove host-issued adapter mutation, genuine callback, authenticated payout report, or approved on-page financing path")
}

func TestKlarnaSandboxEvidenceRejectsWrongMerchantAndProduct(t *testing.T) {
	host := klarnaHostEvidence{MerchantAccount: "seller-a", FinancingMethod: "FIXED_AMOUNT", OrderID: "11111111-1111-1111-1111-111111111111", CaptureID: "22222222-2222-2222-2222-222222222222", RefundID: "33333333-3333-3333-3333-333333333333", MerchantReference1: "order", MerchantReference2: "payment", OrderAmount: 1000, CapturedAmount: 1000, RefundedAmount: 100}
	if !validKlarnaHost(host, "seller-a") {
		t.Fatal("valid host evidence rejected")
	}
	if validKlarnaHost(host, "seller-b") {
		t.Fatal("accepted another merchant's order")
	}
	host.FinancingMethod = "pay_now"
	if validKlarnaHost(host, "seller-a") {
		t.Fatal("accepted an unrelated payment product")
	}
}

func TestKlarnaSandboxRejectsStaleAuthenticatedCapture(t *testing.T) {
	if validKlarnaCapture("capture", 1000, "2020-01-02T03:04:05Z", "capture", 1000, time.Now()) {
		t.Fatal("accepted an old capture returned by the authenticated provider API")
	}
	if !validKlarnaCapture("capture", 1000, time.Now().UTC().Format(time.RFC3339Nano), "capture", 1000, time.Now()) {
		t.Fatal("rejected a recent matching authenticated capture")
	}
}
