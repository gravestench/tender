//go:build sandbox

package sandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/providers/ach"
	"github.com/gravestench/tender/providers/adyen"
	"github.com/gravestench/tender/store"
)

// This protected probe submits only Adyen's published TEST fixtures. The Go
// process handles session setup, while the browser submits directly to Adyen.
func validACHOrigin(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Host != "" && raw == "https://"+u.Host && !mockHost(u.Hostname())
}

func TestACHSandboxSessions(t *testing.T) {
	if os.Getenv("PAYMENTS_ENV") != "sandbox" || os.Getenv("TENDER_ADYEN_API_KEY") == "" || os.Getenv("TENDER_ADYEN_MERCHANT_ACCOUNT") == "" || !strings.HasPrefix(os.Getenv("TENDER_ADYEN_CLIENT_KEY"), "test_") {
		t.Fatal("BLOCKED: select PAYMENTS_ENV=sandbox and supply the enabled Adyen test merchant, API key and public test client key")
	}
	origin := os.Getenv("TENDER_ACH_TEST_ORIGIN")
	if !validACHOrigin(origin) || os.Getenv("TENDER_ACH_GIACT_ENABLED") != "1" {
		t.Fatal("BLOCKED: supply a registered HTTPS test origin and merchant-enabled consumer WEB giact validation")
	}
	merchant := os.Getenv("TENDER_ADYEN_MERCHANT_ACCOUNT")
	adapter, err := adyen.NewACHSandboxQualification(adyen.Config{Environment: tender.EnvironmentSandbox, BaseURL: "https://checkout-test.adyen.com/v72", APIKey: os.Getenv("TENDER_ADYEN_API_KEY"), MerchantAccount: merchant, Origin: origin, ReturnURL: origin + "/tender-ach-qualification"})
	if err != nil {
		t.Fatal("BLOCKED: invalid protected ACH adapter configuration")
	}
	for _, outcome := range []string{"approved", "refused"} {
		t.Run(outcome, func(t *testing.T) {
			key := "ach-qualification-" + outcome + "-" + time.Now().UTC().Format("20060102150405.000000000")
			scope := tender.Scope{MerchantID: "sandbox-qualification", Environment: tender.EnvironmentSandbox}
			market := tender.Market{Country: "US", Currency: "USD", Method: tender.MethodACH}
			engine, err := tender.NewEngine(tender.EngineConfig{Store: store.NewMemory(), Provider: adapter, Scope: scope, Market: market, ProviderName: "adyen", MerchantAccount: merchant})
			if err != nil {
				t.Fatal("BLOCKED: cannot construct protected ACH engine")
			}
			amount := tender.Money{Minor: 1234, Currency: "USD"}
			created, err := engine.Create(t.Context(), tender.Request{PaymentID: key, OrderID: key, Key: key + "/create", Amount: amount})
			if err != nil {
				t.Fatal("BLOCKED: protected adapter did not create an official ACH session")
			}
			var session adyen.ACHSession
			if json.Unmarshal([]byte(created.Payment.Action.Data), &session) != nil || session.ID == "" || session.SessionData == "" || session.Reference == "" || created.Payment.Scope != scope || created.Payment.Market != market || created.Payment.MerchantAccount != merchant {
				t.Fatal("BLOCKED: host session is not bound to the merchant, scope and US ACH product")
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
			sessionInput, _ := json.Marshal(map[string]string{"id": session.ID, "sessionData": session.SessionData})
			cmd.Stdin = bytes.NewReader(sessionInput)
			var browserOutput bytes.Buffer
			cmd.Stdout = &browserOutput
			if err := cmd.Run(); err != nil {
				t.Fatal("BLOCKED: pinned browser did not verify the expected in-page ACH Sessions submission and provider decision")
			}
			var receipt struct {
				SessionResult string `json:"sessionResult"`
			}
			if browserOutput.Len() > 16384 || json.Unmarshal(browserOutput.Bytes(), &receipt) != nil || receipt.SessionResult == "" {
				t.Fatal("BLOCKED: browser did not return a bounded provider session result")
			}
			confirmed, err := engine.Confirm(t.Context(), tender.Request{PaymentID: key, Key: key + "/confirm", Amount: amount, Token: receipt.SessionResult})
			if err != nil || confirmed.Payment.Scope != scope || confirmed.Payment.MerchantAccount != merchant || confirmed.Payment.Market != market || confirmed.Payment.Status != tender.PaymentPending && confirmed.Payment.Status != tender.PaymentDeclined {
				t.Fatal("BLOCKED: official session result did not bind to the original host payment")
			}
			t.Fatal("BLOCKED: provider-signed event, approved mandate and authenticated settlement/return report are still required before ACH qualification can PASS")
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
	if err := freshEvidenceFile(os.Getenv("TENDER_ACH_HOST_EVIDENCE")); err != nil {
		t.Fatal(err)
	}
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
		if err := freshEvidenceFile(os.Getenv(c.env)); err != nil {
			t.Fatal(err)
		}
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
	t.Fatal("BLOCKED: two report rows alone do not prove a host-issued qualified session, approved mandate, signed event and same-order browser path")
}

func TestACHSandboxRejectsMockOrigin(t *testing.T) {
	if !validACHOrigin("https://checkout.merchant.com") {
		t.Fatal("rejected a public HTTPS origin")
	}
	if validACHOrigin("https://127.0.0.1") || validACHOrigin("https://merchant.example.test") {
		t.Fatal("accepted a mock browser origin for official ACH qualification")
	}
}
