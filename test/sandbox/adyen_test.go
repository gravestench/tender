//go:build sandbox

package sandbox_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/netip"
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

func portalMerchant() error {
	if os.Getenv("PAYMENTS_ENV") != "sandbox" {
		return errors.New("BLOCKED: select PAYMENTS_ENV=sandbox")
	}
	merchant := os.Getenv("TENDER_ADYEN_MERCHANT_ACCOUNT")
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`).MatchString(merchant) {
		return errors.New("BLOCKED: supply the scoped Adyen test merchant account")
	}
	return nil
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
	registered, err := url.Parse(origin)
	if err != nil || registered.Scheme != "https" || registered.User != nil || registered.Hostname() == "" || origin != "https://"+registered.Host || mockHost(registered.Hostname()) {
		return nil, fmt.Errorf("BLOCKED: %s must be a registered public HTTPS merchant origin", originName)
	}
	ticket, err := url.Parse(os.Getenv(env))
	if err != nil || ticket.User != nil || ticket.Scheme != "https" || ticket.Scheme+"://"+ticket.Host != origin || !ticketPath.MatchString(ticket.EscapedPath()) || ticket.RawQuery != "" || ticket.ForceQuery || ticket.Fragment != "" {
		return nil, fmt.Errorf("BLOCKED: configure a fresh same-origin %s", env)
	}
	return ticket, nil
}

func mockHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	for _, apex := range []string{"localhost", "test", "invalid", "example", "example.com", "example.org", "example.net"} {
		if host == apex || strings.HasSuffix(host, "."+apex) {
			return true
		}
	}
	return !strings.Contains(host, ".")
}

func regularEvidenceFile(path string) error {
	stat, err := os.Lstat(path)
	if err != nil || !stat.Mode().IsRegular() {
		return errors.New("BLOCKED: supply a regular merchant evidence file")
	}
	return nil
}

func freshProviderTime(observed, now time.Time) bool {
	return !observed.IsZero() && !observed.Before(now.Add(-30*24*time.Hour)) && !observed.After(now.Add(5*time.Minute))
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
	t.Fatal("BLOCKED: browser probe alone does not correlate this host-issued payment with signed provider events and an authenticated report")
}

// Run after the operator captured and refunded the qualified order and Adyen
// produced an accounting export. This check is read-only and can be rerun.
func TestAdyenSandboxPortal(t *testing.T) {
	if err := portalMerchant(); err != nil {
		t.Fatal(err)
	}
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
	rows, err := tx.Query(t.Context(), `SELECT evidence->>'eventCode',evidence->>'pspReference',evidence->>'merchantReference',received_at FROM tender_provider_events WHERE merchant_id='consumer-demo' AND environment='SANDBOX' AND provider='adyen' AND merchant_account=$1 AND processed_at IS NOT NULL AND evidence->>'success'='true' AND evidence#>>'{amount,currency}'='USD' AND (evidence#>>'{amount,value}')::bigint=$2 AND (evidence->>'pspReference'=$3 OR evidence->>'originalReference'=$3)`, merchant, payment.Amount.Minor, payment.ProviderReference)
	if err != nil {
		t.Fatal("cannot read verified provider events")
	}
	psps := map[string]string{}
	var ref string
	for rows.Next() {
		var code, psp, mref string
		var receivedAt time.Time
		if rows.Scan(&code, &psp, &mref, &receivedAt) != nil || !freshProviderTime(receivedAt, time.Now()) {
			t.Fatal("BLOCKED: signed provider event receipt is invalid or stale")
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
	if err := regularEvidenceFile(os.Getenv("TENDER_ADYEN_ACCOUNTING_CSV")); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(os.Getenv("TENDER_ADYEN_ACCOUNTING_CSV"))
	if err != nil {
		t.Fatal("BLOCKED: cannot open Customer Area accounting export")
	}
	defer file.Close()
	if err := compareAccounting(file, merchant, payment.ProviderReference, ref, brand, payment.Amount.Minor, psps); err != nil {
		t.Fatal(err)
	}
	t.Fatal("BLOCKED: read-only accounting comparison alone does not prove a browser checkout on this same order")
}

func compareAccounting(input io.Reader, merchant, paymentPSP, ref, brand string, minor int64, psps map[string]string) error {
	reader := csv.NewReader(input)
	header, err := reader.Read()
	if err != nil {
		return errors.New("invalid accounting CSV")
	}
	index := map[string]int{}
	for _, col := range []string{"Merchant Account", "Psp Reference", "Merchant Reference", "Payment Method", "Record Type", "Payment Currency", "Authorised (PC)", "Captured (PC)", "Modification Psp Reference", "Booking Date (AMS)"} {
		i := slices.Index(header, col)
		if i < 0 || slices.Index(header[i+1:], col) >= 0 {
			return fmt.Errorf("BLOCKED: include report column %q", col)
		}
		index[col] = i
	}
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		return errors.New("BLOCKED: Amsterdam report timezone unavailable")
	}
	now := time.Now()
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
		bookedAt, err := time.ParseInLocation("2006-01-02 15:04:05", get("Booking Date (AMS)"), ams)
		if err != nil || !freshProviderTime(bookedAt, now) {
			return errors.New("BLOCKED: matched accounting row has a stale or invalid Adyen booking date")
		}
		kind := get("Record Type")
		want, ok := expected[kind]
		switch kind {
		case "Received", "AuthenticationRequested":
			if seen[kind] || !registerEquals(get("Authorised (PC)"), 0) || !registerEquals(get("Captured (PC)"), 0) {
				return fmt.Errorf("unexpected register movement on %s", kind)
			}
			seen[kind] = true
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
	const header = "Merchant Account,Psp Reference,Merchant Reference,Payment Method,Record Type,Payment Currency,Authorised (PC),Captured (PC),Modification Psp Reference,Booking Date (AMS)\n"
	const auth = "merchant,payment,order,visa,Authorised,USD,25.99,0.00,\n"
	const capture = "merchant,payment,order,visa,SentForSettle,USD,-25.99,25.99,cap\n"
	const refund = "merchant,payment,order,visa,SentForRefund,USD,0,-25.99,ref\n"
	const refundAuth = "merchant,payment,order,visa,RefundAuthorised,USD,0,0,ref\n"
	const bookedAuth = "merchant,payment,order,visa,RefundAuthorised,USD,-25.99,0,ref\n"
	const releasedAuth = "merchant,payment,order,visa,SentForRefund,USD,25.99,-25.99,ref\n"
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	date := time.Now().In(ams).Format("2006-01-02 15:04:05")
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
			err := compareAccounting(strings.NewReader(header+strings.ReplaceAll(tc.rows, "\n", ","+date+"\n")), "merchant", "payment", "order", "visa", 2599, map[string]string{"CAPTURE": "cap", "REFUND": "ref"})
			if (err == nil) != tc.ok {
				t.Fatalf("accounting comparison error = %v", err)
			}
		})
	}
}

func TestAccountingRequiresFreshProviderBookingOnEveryMatchedRow(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	fresh := time.Now().In(zone).Format("2006-01-02 15:04:05")
	const stale = "2020-01-02 03:04:05"
	const header = "Merchant Account,Psp Reference,Merchant Reference,Payment Method,Record Type,Payment Currency,Authorised (PC),Captured (PC),Modification Psp Reference,Booking Date (AMS)\n"
	rows := func(dates [3]string) string {
		return header +
			"merchant,payment,order,visa,Authorised,USD,25.99,0.00,," + dates[0] + "\n" +
			"merchant,payment,order,visa,SentForSettle,USD,-25.99,25.99,cap," + dates[1] + "\n" +
			"merchant,payment,order,visa,SentForRefund,USD,0,-25.99,ref," + dates[2] + "\n"
	}
	psps := map[string]string{"CAPTURE": "cap", "REFUND": "ref"}
	for _, tc := range []struct {
		name  string
		dates [3]string
		valid bool
	}{
		{"fresh", [3]string{fresh, fresh, fresh}, true},
		{"stale authorization", [3]string{stale, fresh, fresh}, false},
		{"stale capture", [3]string{fresh, stale, fresh}, false},
		{"stale refund", [3]string{fresh, fresh, stale}, false},
		{"future refund", [3]string{fresh, fresh, time.Now().In(zone).Add(6 * time.Minute).Format("2006-01-02 15:04:05")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := compareAccounting(strings.NewReader(rows(tc.dates)), "merchant", "payment", "order", "visa", 2599, psps)
			if (err == nil) != tc.valid {
				t.Fatalf("booking date check = %v", err)
			}
		})
	}
	const noDate = "Merchant Account,Psp Reference,Merchant Reference,Payment Method,Record Type,Payment Currency,Authorised (PC),Captured (PC),Modification Psp Reference\n" +
		"merchant,payment,order,visa,Authorised,USD,25.99,0.00,\n" +
		"merchant,payment,order,visa,SentForSettle,USD,-25.99,25.99,cap\n" +
		"merchant,payment,order,visa,SentForRefund,USD,0,-25.99,ref\n"
	if err := compareAccounting(strings.NewReader(noDate), "merchant", "payment", "order", "visa", 2599, psps); err == nil {
		t.Fatal("accepted accounting rows without a provider booking date")
	}
}

func TestAccountingRejectsDuplicatePreAuthorizationRows(t *testing.T) {
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	date := time.Now().In(ams).Format("2006-01-02 15:04:05")
	const header = "Merchant Account,Psp Reference,Merchant Reference,Payment Method,Record Type,Payment Currency,Authorised (PC),Captured (PC),Modification Psp Reference,Booking Date (AMS)\n"
	base := header +
		"merchant,payment,order,visa,Authorised,USD,25.99,0.00,," + date + "\n" +
		"merchant,payment,order,visa,SentForSettle,USD,-25.99,25.99,cap," + date + "\n" +
		"merchant,payment,order,visa,SentForRefund,USD,0,-25.99,ref," + date + "\n"
	psps := map[string]string{"CAPTURE": "cap", "REFUND": "ref"}
	for _, kind := range []string{"Received", "AuthenticationRequested"} {
		t.Run(kind, func(t *testing.T) {
			row := "merchant,payment,order,visa," + kind + ",USD,0,0,," + date + "\n"
			if err := compareAccounting(strings.NewReader(base+row), "merchant", "payment", "order", "visa", 2599, psps); err != nil {
				t.Fatalf("one %s row rejected: %v", kind, err)
			}
			if err := compareAccounting(strings.NewReader(base+row+row), "merchant", "payment", "order", "visa", 2599, psps); err == nil {
				t.Fatalf("accepted duplicate %s rows", kind)
			}
		})
	}
}

func TestSandboxCaseRoutes(t *testing.T) {
	t.Setenv("TENDER_ADYEN_ORIGIN", "")
	t.Setenv("TENDER_ADYEN_VISA_ORIGIN", "https://checkout.visa.com")
	t.Setenv("TENDER_ADYEN_MC_ORIGIN", "https://checkout.mastercard.com")
	for _, tc := range []struct{ name, env, ticket string }{{"visa_challenge", "TENDER_ADYEN_VISA_CHALLENGE_TICKET", "https://checkout.visa.com/session/guest"}, {"mc_frictionless", "TENDER_ADYEN_MC_FRICTIONLESS_TICKET", "https://checkout.mastercard.com/session/guest"}, {"mc_purchase", "TENDER_ADYEN_MC_PURCHASE_TICKET", "https://checkout.mastercard.com/session/account"}} {
		t.Setenv(tc.env, tc.ticket)
		if _, err := browserTicket(tc.name, tc.env); err != nil {
			t.Errorf("valid independent %s route rejected", tc.name)
		}
	}
	t.Setenv("TENDER_ADYEN_VISA_CHALLENGE_TICKET", "https://checkout.mastercard.com/session/guest")
	if _, err := browserTicket("visa_challenge", "TENDER_ADYEN_VISA_CHALLENGE_TICKET"); err == nil {
		t.Fatal("accepted a ticket for the other brand host")
	}
}

func TestSandboxBrowserRejectsMockOrigin(t *testing.T) {
	t.Setenv("TENDER_ADYEN_VISA_ORIGIN", "https://127.0.0.1")
	t.Setenv("TENDER_ADYEN_VISA_CHALLENGE_TICKET", "https://127.0.0.1/session/guest")
	if _, err := browserTicket("visa_challenge", "TENDER_ADYEN_VISA_CHALLENGE_TICKET"); err == nil {
		t.Fatal("accepted a loopback browser ticket as official merchant evidence")
	}
}

func TestSandboxOriginUsesDomainBoundaries(t *testing.T) {
	if mockHost("notexample.com") || mockHost("notexample.org") || mockHost("notexample.net") {
		t.Fatal("rejected a public domain ending in the reserved apex name")
	}
	for _, host := range []string{"example.com", "sub.example.com", "example.org", "sub.example.net"} {
		if !mockHost(host) {
			t.Fatalf("accepted reserved host %s", host)
		}
	}
}

func TestAdyenPortalPreflightUsesOnlyScopedSecrets(t *testing.T) {
	t.Setenv("PAYMENTS_ENV", "sandbox")
	t.Setenv("TENDER_ADYEN_MERCHANT_ACCOUNT", "merchant")
	t.Setenv("TENDER_ADYEN_API_KEY", "")
	t.Setenv("TENDER_ADYEN_CLIENT_KEY", "")
	t.Setenv("TENDER_ADYEN_HMAC_KEY", "")
	if err := portalMerchant(); err != nil {
		t.Fatalf("read-only portal preflight required browser/API/HMAC secrets: %v", err)
	}
}

func TestAdyenPortalWorkflowScopesSecrets(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/sandbox.yml")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(raw, []byte("      - name: Run adyen_portal\n"))
	if start < 0 {
		t.Fatal("protected portal step missing")
	}
	step := raw[start:]
	if end := bytes.Index(step[len("      - name: Run adyen_portal\n"):], []byte("\n      - name: Run ")); end >= 0 {
		step = step[:len("      - name: Run adyen_portal\n")+end]
	}
	for _, required := range []string{"TENDER_ADYEN_MERCHANT_ACCOUNT:", "TENDER_ADYEN_EVIDENCE_ORDER:", "TENDER_CONSUMER_DATABASE_URL:", "TENDER_ADYEN_ACCOUNTING_CSV_B64:"} {
		if !bytes.Contains(step, []byte(required)) {
			t.Fatalf("portal step lacks %s", required)
		}
	}
	for _, forbidden := range []string{"TENDER_ADYEN_API_KEY:", "TENDER_ADYEN_CLIENT_KEY:", "TENDER_ADYEN_HMAC_KEY:"} {
		if bytes.Contains(step, []byte(forbidden)) {
			t.Fatalf("read-only portal step exposes %s", forbidden)
		}
	}
}

func TestSandboxHarnessRejectsStaleEvidence(t *testing.T) {
	path := t.TempDir() + "/report.csv"
	if err := os.WriteFile(path, []byte("report"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := regularEvidenceFile(path); err != nil {
		t.Fatal(err)
	}
	if freshProviderTime(time.Now().Add(-31*24*time.Hour), time.Now()) {
		t.Fatal("accepted stale provider evidence after CI decoded it to a fresh file")
	}
}

func TestSandboxProviderTimeBounds(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	if !freshProviderTime(now.Add(-24*time.Hour), now) || freshProviderTime(time.Time{}, now) || freshProviderTime(now.Add(-31*24*time.Hour), now) || freshProviderTime(now.Add(6*time.Minute), now) {
		t.Fatal("provider evidence time bounds are wrong")
	}
}

// Every workflow-selected qualification must fail visibly without protected
// merchant access. A PASS or Skip here would make a missing secret look green.
func TestSandboxHarnessOffline(t *testing.T) {
	for _, name := range []string{"TestAdyenSandboxBrowser", "TestAdyenSandboxPortal", "TestPayPalSandboxTransaction", "TestKlarnaSandboxOrderRecord", "TestACHSandboxSessions", "TestACHSandboxReport"} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "go", "test", "-count=1", "-tags=sandbox", "-run", "^"+name+"$", "-v", ".")
			for _, pair := range os.Environ() {
				if !strings.HasPrefix(pair, "TENDER_") && !strings.HasPrefix(pair, "PAYMENTS_ENV=") {
					cmd.Env = append(cmd.Env, pair)
				}
			}
			cmd.Env = append(cmd.Env, "PAYMENTS_ENV=sandbox", "GOPROXY=off")
			output, err := cmd.CombinedOutput()
			if err == nil || !bytes.Contains(output, []byte("BLOCKED:")) || !bytes.Contains(output, []byte("--- FAIL: "+name)) || bytes.Contains(output, []byte("--- SKIP:")) || bytes.Contains(output, []byte("--- PASS:")) {
				t.Fatalf("no-secret %s was not visibly BLOCKED: %s", name, output)
			}
		})
	}
}
