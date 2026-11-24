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

// TestKlarnaSandboxOrderRecord compares a previously purchased, partially
// refunded order in the NA playground with independently exported host values.
// It only reads. A pass establishes order-record agreement for this sample.
func TestKlarnaSandboxOrderRecord(t *testing.T) {
	if os.Getenv("PAYMENTS_ENV") != "sandbox" {
		t.Fatal("BLOCKED: select PAYMENTS_ENV=sandbox and supply the merchant's NA playground order evidence")
	}
	key, path := os.Getenv("TENDER_KLARNA_API_KEY"), os.Getenv("TENDER_KLARNA_HOST_EVIDENCE")
	if !strings.HasPrefix(key, "klarna_test_api_") || path == "" {
		t.Fatal("BLOCKED: supply a Klarna NA playground API key and the host's exported order evidence")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal("BLOCKED: host evidence is unreadable")
	}
	defer f.Close()
	var host struct {
		OrderID, MerchantReference1, MerchantReference2, CaptureID, RefundID, FinancingMethod string
		OrderAmount, CapturedAmount, RefundedAmount                                           int64
	}
	raw, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(raw) > 8192 || json.Unmarshal(raw, &host) != nil {
		t.Fatal("BLOCKED: invalid host evidence")
	}
	uuid := regexp.MustCompile(`(?i)^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
	if !uuid.MatchString(host.OrderID) || !uuid.MatchString(host.CaptureID) || !uuid.MatchString(host.RefundID) || !klarna.IsUSFinancing("US", "USD", host.FinancingMethod) || host.MerchantReference1 == "" || host.MerchantReference2 == "" || host.OrderAmount <= 0 || host.CapturedAmount > host.OrderAmount || host.RefundedAmount <= 0 || host.RefundedAmount >= host.CapturedAmount {
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
}
