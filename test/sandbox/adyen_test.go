//go:build sandbox

package sandbox_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
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
func sandboxMerchant(t *testing.T) {
	t.Helper()
	if os.Getenv("PAYMENTS_ENV") != "sandbox" {
		t.Fatal("BLOCKED: select PAYMENTS_ENV=sandbox")
	}
	var missing []string
	for _, name := range []string{"TENDER_ADYEN_API_KEY", "TENDER_ADYEN_CLIENT_KEY", "TENDER_ADYEN_MERCHANT_ACCOUNT", "TENDER_ADYEN_HMAC_KEY"} {
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
}

var ticketPath = regexp.MustCompile(`^/session/[A-Za-z0-9]+$`)

func browserTicket(name, env string) (*url.URL, error) {
	originName := "TENDER_ADYEN_MC_ORIGIN"
	switch name {
	case "visa_challenge":
		originName = "TENDER_ADYEN_VISA_ORIGIN"
	case "mc_purchase", "mc_frictionless":
	default:
		return nil, errors.New("unknown sandbox case")
	}
	origin := os.Getenv(originName)
	if origin == "" {
		return nil, fmt.Errorf("BLOCKED: configure the registered %s", originName)
	}
	ticket, err := url.Parse(os.Getenv(env))
	if err != nil || ticket.User != nil || ticket.Scheme != "https" || ticket.Scheme+"://"+ticket.Host != origin || !ticketPath.MatchString(ticket.EscapedPath()) || ticket.RawQuery != "" || ticket.ForceQuery || ticket.Fragment != "" {
		return nil, fmt.Errorf("BLOCKED: configure a fresh same-origin %s", env)
	}
	return ticket, nil
}

func TestAdyenSandboxBrowser(t *testing.T) {
	sandboxMerchant(t)
	for _, tc := range []struct{ name, env string }{
		{"visa_challenge", "TENDER_ADYEN_VISA_CHALLENGE_TICKET"},
		{"mc_frictionless", "TENDER_ADYEN_MC_FRICTIONLESS_TICKET"},
		{"mc_purchase", "TENDER_ADYEN_MC_PURCHASE_TICKET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ticket, err := browserTicket(tc.name, tc.env)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "node", "../../examples/consumer/sandbox-card-browser.cjs")
			cmd.Env = []string{"TENDER_ORIGIN=" + ticket.Scheme + "://" + ticket.Host, "TENDER_TICKET=" + ticket.String(), "TENDER_CASE=" + tc.name, "TENDER_EXPECTED_CLIENT_KEY=" + os.Getenv("TENDER_ADYEN_CLIENT_KEY")}
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
	if err != nil || json.Unmarshal(raw, &payment) != nil || payment.Provider != "adyen" || payment.MerchantAccount != merchant || payment.ProviderReference == "" || payment.Market.Country != "US" || payment.Amount.Currency != "USD" || payment.Amount.Minor <= 0 || payment.Status != tender.PaymentCaptured || payment.VerifiedMethod != payment.Market.Method || payment.Funds.Disputed != 0 || payment.Funds.Returned != 0 || payment.Funds.Captured != payment.Amount.Minor || payment.Funds.Refunded != payment.Amount.Minor {
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
		if previous := psps[code]; previous != "" && previous != psp {
			t.Fatal("multiple modification PSPs for a full-amount operation")
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
	var adverse bool
	if err := tx.QueryRow(t.Context(), `SELECT EXISTS(SELECT FROM tender_provider_events WHERE merchant_id='consumer-demo' AND environment='SANDBOX' AND provider='adyen' AND merchant_account=$1
 AND (evidence->>'pspReference'=$2 OR evidence->>'originalReference'=$2) AND (evidence->>'success'='false' OR evidence->>'eventCode' IN ('CAPTURE_FAILED','REFUND_FAILED','REFUNDED_REVERSED','CHARGEBACK')))`, merchant, payment.ProviderReference).Scan(&adverse); err != nil || adverse {
		t.Fatal("provider evidence includes a failed or adverse event")
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
	if err := compareAccounting(file, merchant, payment.ProviderReference, ref, brand, payment.Amount.Minor, psps); err != nil {
		t.Fatal(err)
	}
}

func compareAccounting(input io.Reader, merchant, paymentPSP, ref, brand string, minor int64, psps map[string]string) error {
	reader := csv.NewReader(input)
	header, err := reader.Read()
	if err != nil {
		return errors.New("invalid accounting CSV")
	}
	index := map[string]int{}
	for _, col := range []string{"Merchant Account", "Psp Reference", "Merchant Reference", "Payment Method", "Record Type", "Payment Currency", "Authorised (PC)", "Captured (PC)", "Modification Psp Reference"} {
		i := slices.Index(header, col)
		if i < 0 {
			return fmt.Errorf("BLOCKED: include report column %q", col)
		}
		index[col] = i
	}
	// Captured (PC) is a clearing register. A SentForSettle credits it
	// and Settled releases it. The corresponding refund movements have
	// opposite signs. Use exact decimal arithmetic in payment currency.
	type movement struct {
		auth, capture int64
		psp           string
	}
	expected := map[string]movement{
		"Authorised":       {minor, 0, ""},
		"SentForSettle":    {-minor, minor, psps["CAPTURE"]},
		"RefundAuthorised": {0, 0, psps["REFUND"]},
		"SentForRefund":    {0, -minor, psps["REFUND"]},
		"Settled":          {0, -minor, psps["CAPTURE"]},
		"Refunded":         {0, minor, psps["REFUND"]},
	}
	seen := map[string]bool{}
	var refundAuthBooked, refundAuthReleased bool
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("invalid accounting row")
		}
		get := func(col string) string { return row[index[col]] }
		if get("Merchant Account") != merchant || get("Psp Reference") != paymentPSP {
			continue
		}
		kind := get("Record Type")
		want, ok := expected[kind]
		switch kind {
		case "Received", "AuthenticationRequested":
			if !registerEquals(get("Authorised (PC)"), 0) || !registerEquals(get("Captured (PC)"), 0) {
				return fmt.Errorf("unexpected register movement on %s", kind)
			}
			continue
		}
		if !ok {
			return errors.New("adverse or unrecognized accounting row for this payment")
		}
		// When a refund moves through the authorisation register its full
		// debit must be released by the matching SentForRefund journal.
		if kind == "RefundAuthorised" && registerEquals(get("Authorised (PC)"), -minor) {
			want.auth, refundAuthBooked = -minor, true
		}
		if kind == "SentForRefund" && registerEquals(get("Authorised (PC)"), minor) {
			want.auth, refundAuthReleased = minor, true
		}
		if seen[kind] || get("Merchant Reference") != ref || get("Payment Currency") != "USD" || get("Payment Method") != brand ||
			want.psp != "" && get("Modification Psp Reference") != want.psp || !registerEquals(get("Authorised (PC)"), want.auth) || !registerEquals(get("Captured (PC)"), want.capture) {
			return fmt.Errorf("contradictory %s accounting movement", kind)
		}
		seen[kind] = true
	}
	for _, kind := range []string{"Authorised", "SentForSettle", "SentForRefund"} {
		if !seen[kind] {
			return fmt.Errorf("missing %s accounting movement", kind)
		}
	}
	if refundAuthBooked != refundAuthReleased {
		return errors.New("unbalanced refund authorisation register")
	}
	return nil
}

var fixedPoint = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`)

func registerEquals(raw string, minor int64) bool {
	if raw == "" {
		return minor == 0
	}
	if len(raw) > 40 || !fixedPoint.MatchString(raw) {
		return false
	}
	n, ok := new(big.Rat).SetString(raw)
	return ok && n.Cmp(big.NewRat(minor, 100)) == 0
}

func TestAccountingRegisterEvidence(t *testing.T) {
	const header = "Merchant Account,Psp Reference,Merchant Reference,Payment Method,Record Type,Payment Currency,Authorised (PC),Captured (PC),Modification Psp Reference\n"
	const auth = "merchant,payment,order,visa,Authorised,USD,25.99,0.00,\n"
	const capture = "merchant,payment,order,visa,SentForSettle,USD,-25.99,25.99,cap\n"
	const refund = "merchant,payment,order,visa,SentForRefund,USD,0,-25.99,ref\n"
	const refundAuth = "merchant,payment,order,visa,RefundAuthorised,USD,0,0,ref\n"
	const bookedAuth = "merchant,payment,order,visa,RefundAuthorised,USD,-25.99,0,ref\n"
	const releasedAuth = "merchant,payment,order,visa,SentForRefund,USD,25.99,-25.99,ref\n"
	for _, tc := range []struct {
		name, rows string
		ok         bool
	}{
		{"valid", auth + capture + refund, true},
		{"refund authorisation", auth + capture + refundAuth + refund, true},
		{"refund authorisation register cleared", auth + capture + bookedAuth + releasedAuth, true},
		{"unbalanced refund authorisation", auth + capture + bookedAuth + refund, false},
		{"wrong refund authorisation modification", auth + capture + strings.Replace(refundAuth, ",ref\n", ",other\n", 1) + refund, false},
		{"wrong refund authorisation brand", auth + capture + strings.Replace(refundAuth, ",visa,", ",mc,", 1) + refund, false},
		{"incorrect refund authorisation movement", auth + capture + strings.Replace(refundAuth, ",0,0,", ",0,-25.99,", 1) + refund, false},
		{"incorrect capture", auth + strings.Replace(capture, ",25.99,cap", ",0.01,cap", 1) + refund, false},
		{"incorrect refund sign", auth + capture + strings.Replace(refund, "-25.99", "25.99", 1), false},
		{"nonzero received register", auth + capture + refund + "merchant,payment,order,visa,Received,USD,0,-25.99,\n", false},
		{"duplicate capture", auth + capture + capture + refund, false},
		{"failure", auth + capture + refund + "merchant,payment,order,visa,CaptureFailed,USD,0,-25.99,cap\n", false},
		{"reversal", auth + capture + refund + "merchant,payment,order,visa,RefundedReversed,USD,0,25.99,ref\n", false},
		{"settled register signs", auth + capture + refund + "merchant,payment,order,visa,Settled,USD,0,25.99,cap\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := compareAccounting(strings.NewReader(header+tc.rows), "merchant", "payment", "order", "visa", 2599, map[string]string{"CAPTURE": "cap", "REFUND": "ref"})
			if (err == nil) != tc.ok {
				t.Fatalf("accounting comparison error = %v", err)
			}
		})
	}
}

func TestSandboxCaseRoutes(t *testing.T) {
	t.Setenv("TENDER_ADYEN_ORIGIN", "")
	t.Setenv("TENDER_ADYEN_VISA_ORIGIN", "https://visa.example.test")
	t.Setenv("TENDER_ADYEN_MC_ORIGIN", "https://mc.example.test")
	for _, tc := range []struct{ name, env, ticket string }{{"visa_challenge", "TENDER_ADYEN_VISA_CHALLENGE_TICKET", "https://visa.example.test/session/guest"}, {"mc_frictionless", "TENDER_ADYEN_MC_FRICTIONLESS_TICKET", "https://mc.example.test/session/guest"}, {"mc_purchase", "TENDER_ADYEN_MC_PURCHASE_TICKET", "https://mc.example.test/session/account"}} {
		t.Setenv(tc.env, tc.ticket)
		if _, err := browserTicket(tc.name, tc.env); err != nil {
			t.Errorf("valid independent %s route rejected", tc.name)
		}
	}
	t.Setenv("TENDER_ADYEN_VISA_CHALLENGE_TICKET", "https://mc.example.test/session/guest")
	if _, err := browserTicket("visa_challenge", "TENDER_ADYEN_VISA_CHALLENGE_TICKET"); err == nil {
		t.Fatal("accepted a ticket for the other brand host")
	}
}
