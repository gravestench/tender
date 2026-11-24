//go:build sandbox

package sandbox_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"math/big"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/jackc/pgx/v5"
)

// The suite is explicit and fails visibly when merchant access is missing.
// No browser receives the API key, webhook key, operator token or database DSN.
func sandboxMerchant(t *testing.T) string {
	t.Helper()
	if os.Getenv("PAYMENTS_ENV") != "sandbox" {
		t.Fatal("BLOCKED: select PAYMENTS_ENV=sandbox")
	}
	var missing []string
	for _, name := range []string{"TENDER_ADYEN_API_KEY", "TENDER_ADYEN_CLIENT_KEY", "TENDER_ADYEN_MERCHANT_ACCOUNT", "TENDER_ADYEN_HMAC_KEY", "TENDER_ADYEN_ORIGIN"} {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("BLOCKED: missing %s", strings.Join(missing, ", "))
	}
	if !strings.HasPrefix(os.Getenv("TENDER_ADYEN_CLIENT_KEY"), "test_") {
		t.Fatal("BLOCKED: configure a public Adyen test client key")
	}
	origin := os.Getenv("TENDER_ADYEN_ORIGIN")
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || origin != "https://"+u.Host {
		t.Fatal("BLOCKED: register the controlled HTTPS checkout origin")
	}
	return origin
}

func TestAdyenSandboxBrowser(t *testing.T) {
	origin := sandboxMerchant(t)
	for _, tc := range []struct{ name, env string }{
		{"visa_challenge", "TENDER_ADYEN_VISA_CHALLENGE_TICKET"},
		{"mc_frictionless", "TENDER_ADYEN_MC_FRICTIONLESS_TICKET"},
		{"mc_purchase", "TENDER_ADYEN_MC_PURCHASE_TICKET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ticket, err := url.Parse(os.Getenv(tc.env))
			if err != nil || ticket.User != nil || ticket.Scheme+"://"+ticket.Host != origin || !regexp.MustCompile(`^/session/[A-Za-z0-9]+$`).MatchString(ticket.Path) || ticket.RawQuery != "" || ticket.Fragment != "" {
				t.Fatalf("BLOCKED: configure a fresh same-origin %s", tc.env)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "node", "../../examples/consumer/sandbox-card-browser.cjs")
			cmd.Env = []string{"TENDER_ORIGIN=" + origin, "TENDER_TICKET=" + ticket.String(), "TENDER_CASE=" + tc.name, "TENDER_EXPECTED_CLIENT_KEY=" + os.Getenv("TENDER_ADYEN_CLIENT_KEY")}
			for _, name := range []string{"PATH", "HOME", "TMPDIR", "TENDER_PLAYWRIGHT", "TENDER_CHROMIUM", "TENDER_ADYEN_TEST_ACS_ORIGIN"} {
				if v := os.Getenv(name); v != "" {
					cmd.Env = append(cmd.Env, name+"="+v)
				}
			}
			if err := cmd.Run(); err != nil {
				t.Fatalf("sandbox browser could not verify %s in page; inspect the scoped host and Customer Area records", tc.name)
			}
		})
	}
}

// Run after the operator captured and refunded the qualified order and Adyen
// produced an accounting export. This check is read-only and can be rerun.
func TestAdyenSandboxPortal(t *testing.T) {
	sandboxMerchant(t)
	required := []string{"TENDER_ADYEN_ACCOUNTING_CSV", "TENDER_CONSUMER_DATABASE_URL", "TENDER_CONSUMER_SCHEMA", "TENDER_ADYEN_EVIDENCE_ORDER"}
	for _, name := range required {
		if os.Getenv(name) == "" {
			t.Fatalf("BLOCKED: supply %s from the completed sandbox run", name)
		}
	}
	merchant := os.Getenv("TENDER_ADYEN_MERCHANT_ACCOUNT")
	conn, err := pgx.Connect(t.Context(), os.Getenv("TENDER_CONSUMER_DATABASE_URL"))
	if err != nil {
		t.Fatal("BLOCKED: cannot read the sandbox evidence database")
	}
	defer conn.Close(context.Background())
	tx, err := conn.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal("cannot open read-only evidence transaction")
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), "SET LOCAL search_path TO "+pgx.Identifier{os.Getenv("TENDER_CONSUMER_SCHEMA")}.Sanitize()); err != nil {
		t.Fatal("cannot select evidence schema")
	}
	var raw []byte
	err = tx.QueryRow(t.Context(), `SELECT record FROM tender_payments WHERE merchant_id='consumer-demo' AND environment='SANDBOX' AND order_id=$1`, os.Getenv("TENDER_ADYEN_EVIDENCE_ORDER")).Scan(&raw)
	var payment tender.Payment
	if err != nil || json.Unmarshal(raw, &payment) != nil || payment.Provider != "adyen" || payment.MerchantAccount != merchant || payment.ProviderReference == "" || payment.Market.Country != "US" || payment.Amount.Currency != "USD" || payment.Amount.Minor <= 0 || payment.Funds.Captured != payment.Amount.Minor || payment.Funds.Refunded != payment.Amount.Minor {
		t.Fatal("sandbox payment, capture and refund facts do not match")
	}
	rows, err := tx.Query(t.Context(), `SELECT evidence->>'eventCode',evidence->>'pspReference',evidence->>'merchantReference' FROM tender_provider_events WHERE merchant_id='consumer-demo' AND environment='SANDBOX' AND provider='adyen' AND merchant_account=$1 AND processed_at IS NOT NULL AND evidence->>'success'='true' AND evidence#>>'{amount,currency}'='USD' AND (evidence#>>'{amount,value}')::bigint=$2 AND (evidence->>'pspReference'=$3 OR evidence->>'originalReference'=$3)`, merchant, payment.Amount.Minor, payment.ProviderReference)
	if err != nil {
		t.Fatal("cannot read verified provider events")
	}
	psps := map[string]string{}
	var ref string
	for rows.Next() {
		var code, psp, mref string
		if rows.Scan(&code, &psp, &mref) != nil {
			t.Fatal("invalid event")
		}
		psps[code] = psp
		if code == "AUTHORISATION" {
			ref = mref
		}
	}
	rows.Close()
	if rows.Err() != nil || psps["AUTHORISATION"] != payment.ProviderReference || ref == "" || psps["CAPTURE"] == "" || psps["REFUND"] == "" {
		t.Fatal("missing processed provider authorization, capture or refund")
	}
	var deliveries int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM consumer_fulfillments WHERE merchant_id='consumer-demo' AND environment='SANDBOX' AND payment_id=$1`, payment.ID).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatal("expected one host fulfillment")
	}
	brand := "visa"
	switch payment.Market.Method {
	case tender.MethodVisa:
	case tender.MethodMastercard:
		brand = "mc"
	default:
		t.Fatal("unexpected market")
	}
	file, err := os.Open(os.Getenv("TENDER_ADYEN_ACCOUNTING_CSV"))
	if err != nil {
		t.Fatal("BLOCKED: cannot open Customer Area accounting export")
	}
	defer file.Close()
	reader := csv.NewReader(file)
	header, err := reader.Read()
	if err != nil {
		t.Fatal("invalid accounting CSV")
	}
	index := map[string]int{}
	for _, col := range []string{"Merchant Account", "Psp Reference", "Merchant Reference", "Payment Method", "Record Type", "Payment Currency", "Authorised (PC)", "Modification Psp Reference"} {
		i := slices.Index(header, col)
		if i < 0 {
			t.Fatalf("BLOCKED: include report column %q", col)
		}
		index[col] = i
	}
	var auth, capture, refund bool
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("invalid accounting row")
		}
		get := func(col string) string { return row[index[col]] }
		if get("Merchant Account") != merchant || get("Psp Reference") != payment.ProviderReference || get("Merchant Reference") != ref || get("Payment Currency") != "USD" || get("Payment Method") != brand {
			continue
		}
		switch get("Record Type") {
		case "Authorised":
			v, ok := new(big.Rat).SetString(get("Authorised (PC)"))
			auth = auth || ok && v.Cmp(big.NewRat(payment.Amount.Minor, 100)) == 0
		case "SentForSettle", "Settled":
			capture = capture || get("Modification Psp Reference") == psps["CAPTURE"]
		case "SentForRefund", "Refunded":
			refund = refund || get("Modification Psp Reference") == psps["REFUND"]
		}
	}
	if !auth || !capture || !refund {
		t.Fatalf("portal correlation incomplete: authorization %t, capture %t, refund %t", auth, capture, refund)
	}
}
