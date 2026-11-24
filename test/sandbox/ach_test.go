//go:build sandbox

package sandbox_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gravestench/tender/providers/ach"
)

// This protected probe submits only Adyen's published TEST fixtures. The Go
// process handles session setup, while the browser submits directly to Adyen.
func TestACHSandboxSessions(t *testing.T) {
	if os.Getenv("PAYMENTS_ENV") != "sandbox" || os.Getenv("TENDER_ADYEN_API_KEY") == "" || os.Getenv("TENDER_ADYEN_MERCHANT_ACCOUNT") == "" || !strings.HasPrefix(os.Getenv("TENDER_ADYEN_CLIENT_KEY"), "test_") {
		t.Fatal("BLOCKED: select PAYMENTS_ENV=sandbox and supply the enabled Adyen test merchant, API key and public test client key")
	}
	origin := os.Getenv("TENDER_ACH_TEST_ORIGIN")
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || origin != "https://"+u.Host || os.Getenv("TENDER_ACH_GIACT_ENABLED") != "1" {
		t.Fatal("BLOCKED: supply a registered HTTPS test origin and merchant-enabled consumer WEB giact validation")
	}
	merchant := os.Getenv("TENDER_ADYEN_MERCHANT_ACCOUNT")
	client := &http.Client{Transport: &http.Transport{ForceAttemptHTTP2: true}, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	for _, outcome := range []string{"approved", "refused"} {
		t.Run(outcome, func(t *testing.T) {
			var nonce [16]byte
			rand.Read(nonce[:])
			reference := "tender-ach-" + rand.Text() + "." + base64.RawURLEncoding.EncodeToString(nonce[:])
			t.Logf("sandbox merchant reference %s", reference)
			amount := map[string]any{"value": 1234, "currency": "USD"}
			data, _ := json.Marshal(map[string]any{"merchantAccount": merchant, "reference": reference, "amount": amount, "countryCode": "US", "channel": "Web", "shopperInteraction": "Ecommerce", "allowedPaymentMethods": []string{"ach"}, "mode": "embedded", "returnUrl": origin + "/tender-ach-qualification"})
			req, err := http.NewRequestWithContext(t.Context(), "POST", "https://checkout-test.adyen.com/v72/sessions", bytes.NewReader(data))
			if err != nil {
				t.Fatal("setup failed")
			}
			req.Header.Set("X-API-Key", os.Getenv("TENDER_ADYEN_API_KEY"))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", rand.Text())
			res, err := client.Do(req)
			if err != nil {
				t.Fatal("BLOCKED: cannot contact the Adyen test environment")
			}
			defer res.Body.Close()
			var session struct {
				ID                                    string `json:"id"`
				Data                                  string `json:"sessionData"`
				MerchantAccount, Reference, Mode, URL string
				Amount                                struct {
					Value    int64
					Currency string
				}
			}
			raw, err := io.ReadAll(io.LimitReader(res.Body, 65537))
			if res.StatusCode != 201 || err != nil || len(raw) > 65536 || json.Unmarshal(raw, &session) != nil || session.ID == "" || session.Data == "" || session.Reference != reference || session.MerchantAccount != merchant || session.Mode != "embedded" || session.URL != "" || session.Amount.Currency != "USD" || session.Amount.Value != 1234 {
				t.Fatalf("BLOCKED: ACH-only sandbox session setup failed (HTTP %d)", res.StatusCode)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "node", "ach-browser.cjs")
			cmd.Env = []string{"TENDER_ACH_ORIGIN=" + origin, "TENDER_ACH_CASE=" + outcome, "TENDER_ACH_CLIENT_KEY=" + os.Getenv("TENDER_ADYEN_CLIENT_KEY")}
			for _, key := range []string{"PATH", "HOME", "TMPDIR", "TENDER_CHROMIUM", "TENDER_PLAYWRIGHT"} {
				if v := os.Getenv(key); v != "" {
					cmd.Env = append(cmd.Env, key+"="+v)
				}
			}
			sessionInput, _ := json.Marshal(map[string]string{"id": session.ID, "sessionData": session.Data})
			cmd.Stdin = bytes.NewReader(sessionInput)
			if err := cmd.Run(); err != nil {
				t.Fatal("BLOCKED: pinned browser did not verify the expected in-page ACH Sessions submission and provider decision")
			}
		})
	}
}

// Compare two projected CSV rows from the provider with identifiers exported
// independently from this merchant's host database and the trusted order.
func TestACHSandboxReport(t *testing.T) {
	if os.Getenv("PAYMENTS_ENV") != "sandbox" || os.Getenv("TENDER_ADYEN_MERCHANT_ACCOUNT") == "" {
		t.Fatal("BLOCKED: select PAYMENTS_ENV=sandbox and supply the test merchant account and projected evidence")
	}
	type hostRecord struct {
		Merchant, Reference, PSP, SettlementModification, ReturnModification string
		Minor                                                                int64
	}
	var host hostRecord
	f, err := os.Open(os.Getenv("TENDER_ACH_HOST_EVIDENCE"))
	if err != nil {
		t.Fatal("BLOCKED: supply host evidence for a settled and subsequently returned test debit")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(raw) > 8192 || json.Unmarshal(raw, &host) != nil || host.Merchant != os.Getenv("TENDER_ADYEN_MERCHANT_ACCOUNT") || host.Minor <= 0 {
		t.Fatal("BLOCKED: invalid scoped host evidence")
	}
	for _, c := range []struct {
		env, kind, mod string
		gross          int64
	}{{"TENDER_ACH_SETTLED_CSV", "Settled", host.SettlementModification, host.Minor}, {"TENDER_ACH_RETURNED_CSV", "Chargeback", host.ReturnModification, -host.Minor}} {
		report, err := os.Open(os.Getenv(c.env))
		if err != nil {
			t.Fatalf("BLOCKED: supply %s from the test merchant Settlement details report", c.env)
		}
		l, err := ach.ReadSettlement(report)
		report.Close()
		if err != nil || l.Account != host.Merchant || l.PSP != host.PSP || l.MerchantReference != host.Reference || l.Modification != c.mod || l.Type != c.kind || l.Gross != c.gross {
			t.Fatalf("test merchant %s row differs from trusted host evidence", c.kind)
		}
	}
}
