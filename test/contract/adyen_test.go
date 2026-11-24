//go:build integration

package contract_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/providers/adyen"
	"github.com/gravestench/tender/store"
	"github.com/gravestench/tender/store/postgres"
	"github.com/gravestench/tender/test/contract"
	"github.com/gravestench/tender/webhook"
)

const hmacHex = "44782DEF547AAA06C910C43932B1EB0C71FC68D9D0C057550C48EC2ACF6BA056"
const testAccount = "TestMerchant"
const cardInput = `{"paymentMethod":{"type":"scheme","brand":"visa","holderName":"Synthetic Shopper","encryptedCardNumber":"eyJhbGci.demo.demo.demo.demo","encryptedExpiryMonth":"eyJhbGci.demo.demo.demo.demo","encryptedExpiryYear":"eyJhbGci.demo.demo.demo.demo","encryptedSecurityCode":"eyJhbGci.demo.demo.demo.demo"},"browserInfo":{"acceptHeader":"text/html","colorDepth":24,"javaEnabled":false,"javaScriptEnabled":true,"language":"en-US","screenHeight":900,"screenWidth":1440,"timeZoneOffset":480,"userAgent":"contract"},"shopperEmail":"buyer@example.test","shopperIP":"127.0.0.1"}`

type adyenStore interface {
	tender.Store
	Outbox(context.Context, tender.Scope) ([]tender.OutboxEntry, error)
}
type adyenRig struct {
	t     *testing.T
	e     *tender.Engine
	db    adyenStore
	http  *httptest.Server
	sim   *mockserver.Server
	hook  *webhook.Adyen
	ready chan string
	cfg   adyen.Config
}

func newAdyenRig(t *testing.T, db adyenStore, result, details string) *adyenRig {
	t.Helper()
	h := &adyenRig{t: t, db: db, ready: make(chan string, 16)}
	mux := http.NewServeMux()
	h.http = httptest.NewServer(mux)
	t.Cleanup(h.http.Close)
	key, _ := hex.DecodeString(hmacHex)
	var err error
	h.sim, err = mockserver.New(mockserver.Config{PublicBaseURL: h.http.URL + "/sim", AllowedCallbackTargets: []string{h.http.URL + "/notifications"}, SigningKey: key,
		Routes: mockserver.AdyenRoutes(mockserver.AdyenFixture{MerchantAccount: testAccount, APIKey: "test-only", Brands: []string{"visa", "mc"}, PaymentResult: result, DetailsResult: details, WebhookURL: h.http.URL + "/notifications"})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.sim.Close() })
	mux.Handle("/sim/", http.StripPrefix("/sim", h.sim.Handler()))
	h.cfg = adyen.Config{Environment: tender.EnvironmentMock, BaseURL: h.http.URL + "/sim/v72", APIKey: "test-only", MerchantAccount: testAccount, Origin: h.http.URL, ReturnURL: h.http.URL + "/return", MultiplePartialCaptures: true, Transport: h.http.Client().Transport}
	adapter, err := adyen.New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.e, err = tender.NewEngine(tender.EngineConfig{Store: db, Provider: adapter, Scope: dbScope, Market: dbMarket, ProviderName: "adyen", MerchantAccount: testAccount})
	if err != nil {
		t.Fatal(err)
	}
	h.hook, err = webhook.NewAdyen(webhook.AdyenConfig{Store: db, Engine: h.e, Scope: dbScope, MerchantAccount: testAccount, HMACKeys: []string{hmacHex}, OnStored: func(id string) { h.ready <- id }})
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/notifications", h.hook)
	return h
}
func (h *adyenRig) request(key string, amount int64) tender.Request {
	return tender.Request{PaymentID: "pay", Key: key, Amount: tender.Money{Minor: amount, Currency: "USD"}}
}
func (h *adyenRig) create() tender.Result {
	h.t.Helper()
	r := h.request("create", 100)
	r.OrderID = "order"
	res, err := h.e.Create(h.t.Context(), r)
	if err != nil || res.Payment.Action.Mode != tender.ActionComponent || !strings.Contains(res.Payment.Action.Data, `"visa"`) || strings.Contains(res.Payment.Action.Data, `"mc"`) {
		h.t.Fatalf("card discovery = %+v, %v", res, err)
	}
	return res
}
func (h *adyenRig) confirm() (tender.Result, error) {
	r := h.request("pay", 100)
	r.Token = cardInput
	return h.e.Confirm(h.t.Context(), r)
}
func (h *adyenRig) next() string {
	h.t.Helper()
	select {
	case id := <-h.ready:
		return id
	case <-time.After(4 * time.Second):
		h.t.Fatal("webhook did not reach durable inbox")
	}
	return ""
}
func (h *adyenRig) process() string {
	h.t.Helper()
	id := h.next()
	if err := h.hook.Process(h.t.Context(), id); err != nil {
		h.t.Fatal(err)
	}
	return id
}
func (h *adyenRig) compare(res tender.Result, route string, value int64) {
	h.t.Helper()
	err := contract.Verify(h.t.Context(), h.sim, h.db, dbScope, res.Operation.ID, mockserver.AdyenKey(testAccount, res.Operation.ProviderKey), func(r contract.Records) error {
		var fx mockserver.AdyenRecord
		if err := json.Unmarshal(r.Fixture.Data, &fx); err != nil {
			return err
		}
		if fx.Route != route || fx.Value != value || fx.Currency != "USD" || r.Operation.ProviderKey != res.Operation.ProviderKey || r.Payment.ID != "pay" || r.Payment.MerchantAccount != testAccount {
			return fmt.Errorf("fixture/host disagreement for %s", route)
		}
		if route != "paymentMethods" && (!strings.HasPrefix(fx.Reference, r.Operation.ID+".") || fx.PaymentPSP != r.Payment.ProviderReference) {
			return fmt.Errorf("provider/host reference disagreement for %s", route)
		}
		if route != "paymentMethods" && route != "payments" && fx.PSP != r.Operation.ProviderReference {
			return errors.New("wrong modification PSP")
		}
		if bytes.Contains(r.Fixture.Data, []byte("eyJhbGci")) || bytes.Contains(r.Fixture.Data, []byte("test-only")) {
			return errors.New("fixture retained card input or credential")
		}
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
}
func TestAdyenContract(t *testing.T) {
	run := func(t *testing.T, db adyenStore) {
		h := newAdyenRig(t, db, "Authorised", "Authorised")
		ctx := t.Context()
		created := h.create()
		h.compare(created, "paymentMethods", 100)
		auth, err := h.confirm()
		if err != nil || auth.Payment.Status != tender.PaymentAuthorized || auth.Payment.Funds.Authorized != 100 {
			t.Fatalf("auth = %+v %v", auth, err)
		}
		h.process()
		h.compare(auth, "payments", 100)
		for _, step := range []struct {
			key           string
			amount, total int64
		}{{"first", 40, 40}, {"remainder", 60, 100}} {
			req := h.request(step.key, step.amount)
			res, err := h.e.Capture(ctx, req)
			if err != nil || res.Operation.Status != tender.OperationPending || res.Payment.Funds.Captured == step.total {
				t.Fatalf("capture receipt = %+v %v", res, err)
			}
			id := h.process()
			if err := h.hook.Process(ctx, id); err != nil {
				t.Fatal(err)
			}
			replay, err := h.e.Capture(ctx, req)
			if err != nil || replay.Payment.Funds.Captured != step.total || replay.Operation.Status != tender.OperationSucceeded || replay.Operation.ProviderKey != res.Operation.ProviderKey {
				t.Fatalf("capture replay = %+v %v", replay, err)
			}
			h.compare(replay, "captures", step.amount)
		}
		req := h.request("refund", 25)
		res, err := h.e.Refund(ctx, req)
		if err != nil || res.Payment.Funds.Refunded != 0 {
			t.Fatalf("refund receipt = %+v %v", res, err)
		}
		h.process()
		res, err = h.e.Refund(ctx, req)
		if err != nil || res.Payment.Funds.Refunded != 25 {
			t.Fatalf("refund = %+v %v", res, err)
		}
		h.compare(res, "refunds", 25)
		out, err := db.Outbox(ctx, dbScope)
		if err != nil || len(out) != 1 {
			t.Fatalf("fulfillment = %+v %v", out, err)
		}
		if _, err = h.e.Recover(ctx, res.Operation.ID); !errors.Is(err, tender.ErrUnsupported) {
			t.Fatalf("query = %v", err)
		}
	}
	t.Run("memory", func(t *testing.T) { run(t, store.NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv("TENDER_TEST_DATABASE_URL") == "" {
			t.Skip("set TENDER_TEST_DATABASE_URL for SQL provider contract")
		}
		if !adyenLocalDatabase(os.Getenv("TENDER_TEST_DATABASE_URL")) {
			t.Fatal("Adyen contract requires a loopback IP or private Unix socket for all database hosts")
		}
		run(t, postgres.New(postgresFixture(t)()))
	})
	t.Run("void", func(t *testing.T) {
		h := newAdyenRig(t, store.NewMemory(), "Authorised", "")
		h.create()
		if _, err := h.confirm(); err != nil {
			t.Fatal(err)
		}
		h.process()
		r := h.request("void", 100)
		res, err := h.e.Void(t.Context(), r)
		if err != nil || res.Operation.Status != tender.OperationPending {
			t.Fatal(res, err)
		}
		h.process()
		res, err = h.e.Void(t.Context(), r)
		if err != nil || res.Payment.Status != tender.PaymentVoided {
			t.Fatal(res, err)
		}
		h.compare(res, "cancels", 100)
	})
	for _, tc := range []struct {
		first, details string
		want           tender.PaymentStatus
	}{{"IdentifyShopper", "", tender.PaymentPending}, {"ChallengeShopper", "Authorised", tender.PaymentAuthorized}, {"ChallengeShopper", "RedirectShopper", tender.PaymentPending}, {"RedirectShopper", "", tender.PaymentPending}, {"UnknownAction", "", tender.PaymentPending}, {"Refused", "", tender.PaymentDeclined}} {
		t.Run(tc.first+tc.details, func(t *testing.T) {
			h := newAdyenRig(t, store.NewMemory(), tc.first, tc.details)
			h.create()
			res, err := h.confirm()
			if tc.details != "" {
				if err != nil || res.Payment.Status != tender.PaymentRequiresAction {
					t.Fatal(res, err)
				}
				var action struct{ Action struct{ Token string } }
				if json.Unmarshal([]byte(res.Payment.Action.Data), &action) != nil || action.Action.Token == "" {
					t.Fatal("missing native token")
				}
				r := h.request("details", 100)
				r.Token = fmt.Sprintf(`{"details":{"threeDSResult":%q}}`, action.Action.Token)
				res, err = h.e.Confirm(t.Context(), r)
			}
			if (err != nil && !errors.Is(err, tender.ErrDeclined)) || res.Payment.Status != tc.want || res.Payment.Action.Data != "" {
				t.Fatalf("gated outcome = %+v %v", res, err)
			}
		})
	}
	t.Run("bound endpoints and authentication", func(t *testing.T) {
		h := newAdyenRig(t, store.NewMemory(), "", "")
		for _, env := range []tender.Environment{tender.EnvironmentSandbox, tender.EnvironmentLive, ""} {
			c := h.cfg
			c.Environment = env
			if _, err := adyen.New(c); err == nil {
				t.Fatal("accepted wrong endpoint")
			}
		}
		c := h.cfg
		c.BaseURL = "https://checkout-test.adyen.com/v72"
		if _, err := adyen.New(c); err == nil {
			t.Fatal("MOCK accepted external host")
		}
		c = h.cfg
		c.APIKey = "invalid"
		a, err := adyen.New(c)
		if err != nil {
			t.Fatal(err)
		}
		e, err := tender.NewEngine(tender.EngineConfig{Store: h.db, Provider: a, Scope: dbScope, Market: dbMarket, ProviderName: "adyen", MerchantAccount: testAccount})
		if err != nil {
			t.Fatal(err)
		}
		req := h.request("bad-key", 100)
		req.OrderID = "o"
		if _, err = e.Create(t.Context(), req); !errors.Is(err, tender.ErrAuthentication) {
			t.Fatalf("invalid api key = %v", err)
		}
	})
}

type eventFields struct{ psp, original, account, ref, value, currency, code, success string }

func notification(f eventFields, sign bool) []byte {
	key, _ := hex.DecodeString(hmacHex)
	m := hmac.New(sha256.New, key)
	fmt.Fprint(m, strings.Join([]string{f.psp, f.original, f.account, f.ref, f.value, f.currency, f.code, f.success}, ":"))
	sig := base64.StdEncoding.EncodeToString(m.Sum(nil))
	if !sign {
		sig = "bad"
	}
	b, _ := json.Marshal(map[string]any{"live": "false", "notificationItems": []any{map[string]any{"NotificationRequestItem": map[string]any{"pspReference": f.psp, "originalReference": f.original, "merchantAccountCode": f.account, "merchantReference": f.ref, "amount": map[string]any{"currency": f.currency, "value": json.Number(f.value)}, "eventCode": f.code, "success": f.success, "additionalData": map[string]any{"hmacSignature": sig}}}}})
	return b
}
func (h *adyenRig) post(body []byte) int {
	h.t.Helper()
	resp, err := h.http.Client().Post(h.http.URL+"/notifications", "application/json", bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
func TestAdyenWebhookReplay(t *testing.T) {
	h := newAdyenRig(t, store.NewMemory(), "Authorised", "")
	official := []byte(`{"live":"false","notificationItems":[{"NotificationRequestItem":{"pspReference":"7914073381342284","merchantAccountCode":"TestMerchant","merchantReference":"TestPayment-1407325143704","amount":{"value":1130,"currency":"EUR"},"eventCode":"AUTHORISATION","success":"true","additionalData":{"hmacSignature":"coqCmt/IZ4E3CzPvMY8zTjQVL5hYJUiBRg8UU+iCWo0="}}}]}`)
	if h.post(official) != 202 {
		t.Fatal("official standard HMAC vector rejected")
	}
	id := h.next()
	if err := h.hook.Process(t.Context(), id); !errors.Is(err, tender.ErrNotFound) {
		t.Fatalf("unresolved receipt = %v", err)
	}
	if h.post(bytes.Replace(official, []byte("1130"), []byte("1131"), 1)) != 401 {
		t.Fatal("accepted tampered amount")
	}
	if h.post(bytes.Replace(official, []byte(`"false"`), []byte(`"true"`), 1)) < 400 {
		t.Fatal("accepted wrong environment")
	}
	h.create()
	auth, err := h.confirm()
	if err != nil {
		t.Fatal(err)
	}
	h.process()
	rec, _ := h.sim.Lookup(mockserver.AdyenKey(testAccount, auth.Operation.ProviderKey))
	var fx mockserver.AdyenRecord
	if err := json.Unmarshal(rec.Data, &fx); err != nil {
		t.Fatal(err)
	}
	base := eventFields{auth.Payment.ProviderReference, "", testAccount, fx.Reference, "100", "USD", "AUTHORISATION", "true"}
	if h.post(notification(base, false)) != 401 {
		t.Fatal("accepted invalid signature")
	}
	for _, change := range []func(*eventFields){func(f *eventFields) { f.account = "Other" }, func(f *eventFields) { f.psp = "OTHERPSP" }, func(f *eventFields) { f.ref = "missing-op" }, func(f *eventFields) { f.value = "99" }, func(f *eventFields) { f.currency = "EUR" }, func(f *eventFields) { f.original = "wrong" }, func(f *eventFields) { f.code = "CAPTURE" }} {
		f := base
		change(&f)
		code := h.post(notification(f, true))
		if code == 202 {
			if err = h.hook.Process(t.Context(), h.next()); err == nil {
				t.Fatalf("applied mismatched notification %+v", f)
			}
		}
		p, err := h.e.Get(t.Context(), "pay")
		if err != nil || p.Funds != (tender.Funds{Authorized: 100}) {
			t.Fatal(p, err)
		}
	}
}
func TestAdyenObservationRace(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprintf("webhookBeforeLostResponse=%v", race), func(t *testing.T) {
			h := newAdyenRig(t, store.NewMemory(), "Authorised", "")
			h.create()
			if _, err := h.confirm(); err != nil {
				t.Fatal(err)
			}
			h.process()
			delay := time.Duration(0)
			if race {
				delay = 200 * time.Millisecond
			}
			if err := h.sim.SetScenarios("adyen.captures", mockserver.Scenario{Fault: mockserver.LoseResponse, Delay: delay, DuplicateCallbacks: true}); err != nil {
				t.Fatal(err)
			}
			req := h.request("capture", 100)
			ch := make(chan error, 1)
			go func() { _, err := h.e.Capture(t.Context(), req); ch <- err }()
			if !race {
				if err := <-ch; !errors.Is(err, tender.ErrUnknownOutcome) {
					t.Fatalf("lost reply = %v", err)
				}
			}
			h.process()
			h.process()
			if race {
				if err := <-ch; err != nil {
					t.Fatalf("stale transport failure won = %v", err)
				}
			}
			res, err := h.e.Capture(t.Context(), req)
			if err != nil || res.Operation.Status != tender.OperationSucceeded || res.Payment.Funds.Captured != 100 {
				t.Fatalf("recovered = %+v %v", res, err)
			}
			h.compare(res, "captures", 100)
			out, err := h.db.Outbox(t.Context(), dbScope)
			if err != nil || len(out) != 1 {
				t.Fatal(out, err)
			}
		})
	}
}

// A store's operation IDs are only unique within its scope. A signed reference
// from another payment/attempt must still fail if a host reuses an ID.
func TestAdyenObservationRaceScopedReference(t *testing.T) {
	source := newAdyenRig(t, store.NewMemory(), "Authorised", "")
	source.create()
	original, err := source.confirm()
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := source.sim.Lookup(mockserver.AdyenKey(testAccount, original.Operation.ProviderKey))
	if !ok {
		t.Fatal("missing payment fixture")
	}
	var fx mockserver.AdyenRecord
	if err := json.Unmarshal(rec.Data, &fx); err != nil {
		t.Fatal(err)
	}
	body := notification(eventFields{original.Payment.ProviderReference, "", testAccount, fx.Reference, "100", "USD", "AUTHORISATION", "true"}, true)
	for _, variant := range []string{"payment", "attempt", "operation"} {
		t.Run(variant, func(t *testing.T) {
			target := newAdyenRig(t, store.NewMemory(), "", "")
			p := original.Payment
			op := original.Operation
			if variant == "payment" {
				p.ID = "other-payment"
				op.PaymentID = p.ID
			}
			if variant == "attempt" {
				p.AttemptID = "other-attempt"
				op.AttemptID = p.AttemptID
			}
			if variant == "operation" {
				op.ID = "different-operation"
			}
			p.ProviderReference = ""
			p.Snapshot = tender.Snapshot{Status: tender.PaymentCreated}
			p.Version = 1
			op.Status = tender.OperationReserved
			op.BaseVersion = 1
			op.ProviderReference = ""
			if err := target.db.Transact(t.Context(), func(tx tender.Tx) error {
				if err := tx.InsertPayment(p); err != nil {
					return err
				}
				return tx.InsertOperation(tender.OperationRecord{Operation: op, PaymentVersion: 1})
			}); err != nil {
				t.Fatal(err)
			}
			if code := target.post(body); code != 202 {
				t.Fatal(code)
			}
			if err := target.hook.Process(t.Context(), target.next()); err == nil {
				t.Fatal("valid signature applied to different " + variant)
			}
			got, err := target.e.Get(t.Context(), p.ID)
			if err != nil || got.Funds != (tender.Funds{}) {
				t.Fatal(got, err)
			}
		})
	}
}

func TestAdyenContractOfflineDatabase(t *testing.T) {
	for _, dsn := range []string{"host=database.example.invalid dbname=fixture", "host=127.0.0.1,192.0.2.1 dbname=fixture"} {
		if adyenLocalDatabase(dsn) {
			t.Fatal("accepted remote database endpoint")
		}
	}
	if !adyenLocalDatabase("host=/private/tmp/fixture dbname=fixture") {
		t.Fatal("private socket rejected")
	}
}
func adyenLocalDatabase(dsn string) bool {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return false
	}
	local := func(host string) bool {
		ip, err := netip.ParseAddr(host)
		return filepath.IsAbs(host) || err == nil && ip.Zone() == "" && ip.Unmap().IsLoopback()
	}
	if !local(cfg.Host) {
		return false
	}
	for _, fallback := range cfg.Fallbacks {
		if !local(fallback.Host) {
			return false
		}
	}
	return true
}

func TestAdyenContractPinnedWebInputAndActivation(t *testing.T) {
	h := newAdyenRig(t, store.NewMemory(), "Authorised", "")
	h.create()
	r := h.request("sdk", 100)
	r.Token = strings.Replace(cardInput, `"javaScriptEnabled":true,`, "", 1)
	if got, err := h.e.Confirm(t.Context(), r); err != nil || got.Payment.Status != tender.PaymentAuthorized {
		t.Errorf("pinned SDK browserInfo = %s %v", got.Payment.Status, err)
	}
	c := h.cfg
	c.Environment = tender.EnvironmentLive
	c.BaseURL = "https://example-checkout-live.adyenpayments.com/checkout/v72"
	c.Origin = "https://merchant.example"
	c.ReturnURL = c.Origin + "/return"
	if _, err := adyen.New(c); err == nil {
		t.Error("accepted LIVE before merchant activation")
	}
	c.LiveActivated = true
	c.BaseURL = "https://1797a841fbb37ca7-AdyenDemo-checkout-live.adyenpayments.com/checkout/v72"
	if _, err := adyen.New(c); err != nil {
		t.Error("documented live merchant hostname rejected", err)
	}
	c.BaseURL = "https://1797a841fbb37ca7-AdyenDemo-checkout-live.adyenpayments.com.example.invalid/checkout/v72"
	if _, err := adyen.New(c); err == nil {
		t.Error("accepted lookalike live hostname")
	}

}
func TestAdyenContractRejectsRawOrUnboundInput(t *testing.T) {
	h := newAdyenRig(t, store.NewMemory(), "ChallengeShopper", "Authorised")
	h.create()
	for i, token := range []string{strings.Replace(cardInput, "eyJhbGci.demo.demo.demo.demo", "123", 1), strings.Replace(cardInput, `"type":"scheme"`, `"number":"123","type":"scheme"`, 1), strings.Replace(cardInput, `"brand":"visa"`, `"brand":"mc"`, 1)} {
		r := h.request(fmt.Sprintf("invalid-%d", i), 100)
		r.Token = token
		res, err := h.e.Confirm(t.Context(), r)
		if !errors.Is(err, tender.ErrValidation) {
			t.Fatal(err)
		}
		if _, ok := h.sim.Lookup(mockserver.AdyenKey(testAccount, res.Operation.ProviderKey)); ok {
			t.Fatal("invalid input dispatched")
		}
	}
	res, err := h.confirm()
	if err != nil {
		t.Fatal(err)
	}
	r := h.request("injected-details", 100)
	r.Token = `{"details":{"redirectResult":"untrusted"},"merchantAccount":"Other"}`
	if _, err = h.e.Confirm(t.Context(), r); !errors.Is(err, tender.ErrValidation) {
		t.Fatal(err)
	}
	if res.Payment.Action.Mode != tender.ActionComponent {
		t.Fatal("native action missing")
	}
}
func TestAdyenWebhookReplayOriginalReference(t *testing.T) {
	h := newAdyenRig(t, store.NewMemory(), "Authorised", "")
	h.create()
	auth, err := h.confirm()
	if err != nil {
		t.Fatal(err)
	}
	h.process()
	rec, _ := h.sim.Lookup(mockserver.AdyenKey(testAccount, auth.Operation.ProviderKey))
	var fx mockserver.AdyenRecord
	if err := json.Unmarshal(rec.Data, &fx); err != nil {
		t.Fatal(err)
	}
	capReq := h.request("capture", 100)
	capResult, err := h.e.Capture(t.Context(), capReq)
	if err != nil {
		t.Fatal(err)
	}
	// Deliver the event with the original payment reference first; the modification
	// PSP is already bound by the HTTP acknowledgment. Repeat the equivalent
	// event with the modification reference, as a separate signed record.
	body := notification(eventFields{capResult.Operation.ProviderReference, auth.Payment.ProviderReference, testAccount, fx.Reference, "100", "USD", "CAPTURE", "true"}, true)
	fixtureID := h.next()
	if h.post(body) != 202 {
		t.Fatal("signed event rejected")
	}
	h.process()
	if err := h.hook.Process(t.Context(), fixtureID); err != nil {
		t.Fatal(err)
	}
	if h.post(body) != 202 {
		t.Fatal("duplicate event rejected")
	}
	h.process()
	got, err := h.e.Get(t.Context(), "pay")
	if err != nil || got.Funds.Captured != 100 {
		t.Fatal(got, err)
	}
	out, err := h.db.Outbox(t.Context(), dbScope)
	if err != nil || len(out) != 1 {
		t.Fatal(out, err)
	}
}

func TestAdyenWebhookReplayPendingDetails(t *testing.T) {
	h := newAdyenRig(t, store.NewMemory(), "ChallengeShopper", "Pending")
	h.create()
	first, err := h.confirm()
	if err != nil {
		t.Fatal(err)
	}
	var action adyen.CardAction
	if err := json.Unmarshal([]byte(first.Payment.Action.Data), &action); err != nil {
		t.Fatal(err)
	}
	details := h.request("details", 100)
	details.Token = fmt.Sprintf(`{"details":{"threeDSResult":%q}}`, action.Action.Token)
	pending, err := h.e.Confirm(t.Context(), details)
	if err != nil || pending.Operation.Status != tender.OperationPending {
		t.Fatal(pending, err)
	}
	if h.post(notification(eventFields{pending.Payment.ProviderReference, "", testAccount, action.Reference, "100", "USD", "AUTHORISATION", "true"}, true)) != 202 {
		t.Fatal("signed final authorization rejected")
	}
	if err := h.hook.Process(t.Context(), h.next()); !errors.Is(err, tender.ErrConflict) {
		t.Fatal("webhook alone authorized unverified brand", err)
	}
	replay, err := h.e.Confirm(t.Context(), details)
	if err != nil || replay.Operation.Status != tender.OperationPending || replay.Payment.Funds.Authorized != 0 {
		t.Fatal(replay, err)
	}

}
func TestAdyenContractWireReplay(t *testing.T) {
	h := newAdyenRig(t, store.NewMemory(), "Authorised", "")
	created := h.create()
	res, err := h.confirm()
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := adyen.New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := tender.ProviderRequest{Payment: created.Payment, Operation: res.Operation, Token: cardInput}
	got, err := adapter.Execute(t.Context(), r)
	if err != nil || got.ProviderReference != res.Payment.ProviderReference {
		t.Fatal(got, err)
	}
	r.Token = strings.Replace(cardInput, "Synthetic Shopper", "Changed Shopper", 1)
	if replay, err := adapter.Execute(t.Context(), r); err != nil || replay.ProviderReference != got.ProviderReference {
		t.Fatal("idempotent replay did not return original provider decision", err)
	}
}

type changeAdyenReply func(*http.Request) (*http.Response, error)

func (f changeAdyenReply) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func (h *adyenRig) engine(c adyen.Config, now func() time.Time) *tender.Engine {
	h.t.Helper()
	a, err := adyen.New(c)
	if err != nil {
		h.t.Fatal(err)
	}
	e, err := tender.NewEngine(tender.EngineConfig{Store: h.db, Scope: dbScope, Market: dbMarket, ProviderName: "adyen", MerchantAccount: testAccount, Provider: a, Now: now})
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}
func TestAdyenContractAuthoritativeBinding(t *testing.T) {
	for _, bad := range []string{"echo", "missing ref", "missing amount", "missing brand", "different brand", "non-scheme", "missing browser brand", "incomplete challenge"} {
		t.Run(bad, func(t *testing.T) {
			result := "Authorised"
			if bad == "incomplete challenge" {
				result = "ChallengeShopper"
			}
			h := newAdyenRig(t, store.NewMemory(), result, "")
			h.create()
			cfg := h.cfg
			cfg.Transport = changeAdyenReply(func(req *http.Request) (*http.Response, error) {
				resp, err := h.cfg.Transport.RoundTrip(req)
				if err != nil {
					return nil, err
				}
				// These fields are specified by the v72 authorized-response schema.
				var body map[string]any
				err = json.NewDecoder(resp.Body).Decode(&body)
				resp.Body.Close()
				if err != nil {
					return nil, err
				}
				body["paymentMethod"] = map[string]any{"type": "scheme", "brand": "visa"}
				switch bad {
				case "echo":
					resp.Header.Set("Idempotency-Key", "other")
				case "missing ref":
					delete(body, "merchantReference")
				case "missing amount":
					delete(body, "amount")
				case "missing brand":
					delete(body, "paymentMethod")
				case "different brand":
					body["paymentMethod"] = map[string]any{"type": "scheme", "brand": "mc"}
				case "non-scheme":
					body["paymentMethod"] = map[string]any{"type": "paypal", "brand": "visa"}
				case "incomplete challenge":
					delete(body["action"].(map[string]any), "authorisationToken")
				}
				raw, _ := json.Marshal(body)
				resp.Body = io.NopCloser(bytes.NewReader(raw))
				return resp, nil
			})
			h.e = h.engine(cfg, nil)
			r := h.request("pay", 100)
			r.Token = cardInput
			if bad == "missing browser brand" {
				r.Token = strings.Replace(r.Token, `"brand":"visa",`, "", 1)
			}
			got, err := h.e.Confirm(t.Context(), r)
			if got.Payment.Funds.Authorized != 0 || got.Payment.Action.Mode == tender.ActionComponent && got.Payment.Action.Data != "" && strings.Contains(got.Payment.Action.Data, "threeDS2") {
				t.Fatalf("accepted %s, status %s error %v", bad, got.Payment.Status, err)
			}
			if bad == "missing browser brand" {
				if !errors.Is(err, tender.ErrValidation) {
					t.Fatal(err)
				}
				return
			}
			if bad == "incomplete challenge" {
				return
			}
			// The unsigned network in the otherwise valid signed webhook cannot repair
			// missing or contradictory authenticated HTTP evidence.
			if err := h.hook.Process(t.Context(), h.next()); !errors.Is(err, tender.ErrConflict) {
				t.Fatalf("admitted webhook without brand evidence: %v", err)
			}
		})
	}
}
func TestAdyenWebhookReplayCaptureFailure(t *testing.T) {
	for _, timing := range []string{"first processed", "stored first", "late"} {
		t.Run(timing, func(t *testing.T) {
			db := store.NewMemory()
			h := newAdyenRig(t, db, "Authorised", "")
			h.create()
			auth, err := h.confirm()
			if err != nil {
				t.Fatal(err)
			}
			h.process()
			req := h.request("cap", 100)
			cap, err := h.e.Capture(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			capEvent := h.next()
			rec, _ := h.sim.Lookup(mockserver.AdyenKey(testAccount, cap.Operation.ProviderKey))
			var fx mockserver.AdyenRecord
			json.Unmarshal(rec.Data, &fx)
			if timing == "late" {
				if err := h.hook.Process(t.Context(), capEvent); err != nil {
					t.Fatal(err)
				}
			}
			fail := notification(eventFields{fx.PSP, auth.Payment.ProviderReference, testAccount, fx.Reference, "100", "USD", "CAPTURE_FAILED", "true"}, true)
			if h.post(fail) != 202 {
				t.Fatal("signed failure rejected")
			}
			failedEvent := h.next()
			if timing == "stored first" {
				_ = h.hook.Process(t.Context(), capEvent)
				out, _ := db.Outbox(t.Context(), dbScope)
				if len(out) != 0 {
					t.Fatal("stored failure did not fence capture")
				}
			}
			if err := h.hook.Process(t.Context(), failedEvent); err != nil {
				t.Fatal(err)
			}
			if err := h.hook.Process(t.Context(), capEvent); err != nil {
				t.Fatal(err)
			}
			p, err := h.e.Get(t.Context(), "pay")
			if err != nil || p.Status != "capture_failed" {
				t.Fatal(p, err)
			}
			out, err := db.Outbox(t.Context(), dbScope)
			if err != nil {
				t.Fatal(err)
			}
			if timing == "late" {
				if len(out) != 1 || p.Funds.Captured != 100 {
					t.Fatal("lost prior capture audit")
				}
			} else if len(out) != 0 {
				t.Fatal("failed capture fulfilled")
			}
		})
	}
}
func TestAdyenWebhookReplayDistinctCaptureFailures(t *testing.T) {
	db := store.NewMemory()
	h := newAdyenRig(t, db, "Authorised", "")
	h.create()
	if _, err := h.confirm(); err != nil {
		t.Fatal(err)
	}
	h.process()
	capture := func(key string, minor int64) (tender.Result, string) {
		res, err := h.e.Capture(t.Context(), h.request(key, minor))
		if err != nil {
			t.Fatal(err)
		}
		return res, h.next()
	}
	storeFailure := func(cap tender.Result) string {
		rec, _ := h.sim.Lookup(mockserver.AdyenKey(testAccount, cap.Operation.ProviderKey))
		var fx mockserver.AdyenRecord
		if err := json.Unmarshal(rec.Data, &fx); err != nil {
			t.Fatal(err)
		}
		raw := notification(eventFields{fx.PSP, fx.PaymentPSP, testAccount, fx.Reference, fmt.Sprint(cap.Operation.Amount.Minor), "USD", "CAPTURE_FAILED", "true"}, true)
		if h.post(raw) != 202 {
			t.Fatal("failure rejected")
		}
		return h.next()
	}
	first, firstOK := capture("partial", 40)
	if err := h.hook.Process(t.Context(), firstOK); err != nil {
		t.Fatal(err)
	}
	second, secondOK := capture("remainder", 60)
	firstFailed := storeFailure(first)
	if err := h.hook.Process(t.Context(), secondOK); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("unprocessed failure failed to fence other capture: %v", err)
	}
	for _, id := range []string{firstFailed, secondOK} {
		if err := h.hook.Process(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.hook.Process(t.Context(), storeFailure(second)); err != nil {
		t.Fatal(err)
	}
	notices, err := db.CaptureFailures(t.Context(), dbScope)
	if err != nil || len(notices) != 2 || notices[0].OperationID != first.Operation.ID || notices[1].OperationID != second.Operation.ID {
		t.Fatalf("distinct adverse notices: %+v %v", notices, err)
	}
	out, err := db.Outbox(t.Context(), dbScope)
	if err != nil || len(out) != 0 {
		t.Fatalf("fulfilled held captures: %+v %v", out, err)
	}
}

func TestAdyenContractRecoverLostModification(t *testing.T) {
	h := newAdyenRig(t, store.NewMemory(), "Authorised", "")
	h.create()
	auth, err := h.confirm()
	if err != nil {
		t.Fatal(err)
	}
	h.process()
	if err := h.sim.SetScenarios("adyen.captures", mockserver.Scenario{Fault: mockserver.LoseResponse}); err != nil {
		t.Fatal(err)
	}
	r := h.request("lost", 100)
	lost, err := h.e.Capture(t.Context(), r)
	if !errors.Is(err, tender.ErrUnknownOutcome) {
		t.Fatal(err)
	}
	_ = h.next()
	rec, _ := h.sim.Lookup(mockserver.AdyenKey(testAccount, lost.Operation.ProviderKey))
	var cap mockserver.AdyenRecord
	json.Unmarshal(rec.Data, &cap)
	rec, _ = h.sim.Lookup(mockserver.AdyenKey(testAccount, auth.Operation.ProviderKey))
	var pay mockserver.AdyenRecord
	json.Unmarshal(rec.Data, &pay)
	body := notification(eventFields{cap.PSP, cap.PaymentPSP, testAccount, pay.Reference, "100", "USD", "CAPTURE", "true"}, true)
	if h.post(body) != 202 {
		t.Fatal("event rejected")
	}
	id := h.next()
	if err := h.hook.Process(t.Context(), id); err == nil {
		t.Fatal("guessed modification identity")
	}
	restarted := h.engine(h.cfg, nil)
	result, err := restarted.Recover(t.Context(), lost.Operation.ID)
	if err != nil || result.Operation.Status != tender.OperationPending || result.Operation.ProviderReference != cap.PSP {
		t.Fatalf("same-key recovery = %s %v", result.Operation.Status, err)
	}
	if err := h.hook.Process(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	result, err = restarted.Capture(t.Context(), r)
	if err != nil || result.Payment.Funds.Captured != 100 {
		t.Fatal(result, err)
	}
}

func TestAdyenContractCompanyScopedReplay(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	sim, err := mockserver.New(mockserver.Config{PublicBaseURL: srv.URL, Routes: func(s *mockserver.Server) http.Handler {
		m := http.NewServeMux()
		for account, network := range map[string]string{"First": "visa", "Second": "mc"} {
			routes := mockserver.AdyenRoutes(mockserver.AdyenFixture{CompanyAccount: "Company", MerchantAccount: account, APIKey: account + "-key", Brands: []string{network}})(s)
			m.Handle("/"+account+"/", http.StripPrefix("/"+account, routes))
		}
		return m
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()
	mux.Handle("/", sim.Handler())
	for _, account := range []string{"First", "Second"} {
		raw := fmt.Sprintf(`{"merchantAccount":%q,"amount":{"value":100,"currency":"USD"},"channel":"Web","countryCode":"US","allowedPaymentMethods":["scheme"]}`, account)
		req, _ := http.NewRequest("POST", srv.URL+"/"+account+"/v72/paymentMethods", strings.NewReader(raw))
		req.Header.Set("X-API-Key", account+"-key")
		req.Header.Set("Idempotency-Key", "same-company-key")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || resp.Header.Get("Idempotency-Key") != "same-company-key" || !bytes.Contains(body, []byte(`"visa"`)) {
			t.Errorf("company-scoped replay for %s: %d key=%q body=%s", account, resp.StatusCode, resp.Header.Get("Idempotency-Key"), body)
		}
	}
	if _, found := sim.Lookup(mockserver.AdyenKey("Second", "same-company-key")); found {
		t.Fatal("one company key committed a second merchant effect")
	}
}

func TestAdyenContractLegacyAuthorizationCannotCapture(t *testing.T) {
	h := newAdyenRig(t, store.NewMemory(), "Authorised", "")
	created := h.create()
	auth, err := h.confirm()
	if err != nil {
		t.Fatal(err)
	}
	h.process()
	// Simulate a payment restored from before the adapter stored card-network
	// evidence. A PSP and authorized principal alone cannot establish its rail.
	raw, _ := json.Marshal(auth.Payment)
	var fields map[string]json.RawMessage
	json.Unmarshal(raw, &fields)
	delete(fields, "VerifiedMethod")
	raw, _ = json.Marshal(fields)
	var legacy tender.Payment
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	adapter, err := adyen.New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	op := created.Operation
	op.ID = "legacy-cap"
	op.ProviderKey = "legacy-key"
	op.Kind = tender.OpCapture
	if result, err := adapter.Execute(t.Context(), tender.ProviderRequest{Payment: legacy, Operation: op}); err == nil {
		t.Fatalf("legacy unverified capture dispatched: %v", result.Status)
	}
}
func TestAdyenContractRecoveryBounds(t *testing.T) {
	h := newAdyenRig(t, store.NewMemory(), "Authorised", "")
	h.create()
	if _, err := h.confirm(); err != nil {
		t.Fatal(err)
	}
	h.process()
	if err := h.sim.SetScenarios("adyen.captures", mockserver.Scenario{Fault: mockserver.LoseResponse}); err != nil {
		t.Fatal(err)
	}
	lost, err := h.e.Capture(t.Context(), h.request("lost", 100))
	if !errors.Is(err, tender.ErrUnknownOutcome) {
		t.Fatal(err)
	}
	_ = h.next()
	cfg := h.cfg
	cfg.BaseURL = h.http.URL + "/other/v72"
	cfg.Transport = changeAdyenReply(func(*http.Request) (*http.Response, error) {
		t.Fatal("recovery sent to new origin or outside window")
		return nil, errors.New("unreachable")
	})
	if _, err := h.engine(cfg, nil).Recover(t.Context(), lost.Operation.ID); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("changed route %v", err)
	}
	cfg.BaseURL = h.cfg.BaseURL
	if _, err := h.engine(cfg, func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }).Recover(t.Context(), lost.Operation.ID); !errors.Is(err, tender.ErrUnsupported) {
		t.Fatalf("expired replay %v", err)
	}
	if _, err := h.engine(cfg, func() time.Time { return time.Now().Add(-48 * time.Hour) }).Recover(t.Context(), lost.Operation.ID); !errors.Is(err, tender.ErrUnsupported) {
		t.Fatalf("clock moved back before reservation %v", err)
	}
}
func TestAdyenWebhookReplayCaptureFailureSQL(t *testing.T) {
	dsn := os.Getenv("TENDER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("disposable local PostgreSQL required")
	}
	if !adyenLocalDatabase(dsn) {
		t.Fatal("local database required")
	}
	open := postgresFixture(t)
	db := open()
	h := newAdyenRig(t, postgres.New(db), "Authorised", "")
	h.create()
	if _, err := h.confirm(); err != nil {
		t.Fatal(err)
	}
	h.process()
	req := h.request("cap", 100)
	cap, err := h.e.Capture(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	h.process()
	if _, err := db.ExecContext(t.Context(), "UPDATE tender_outbox SET consumed_at=CURRENT_TIMESTAMP"); err != nil {
		t.Fatal(err)
	}
	rec, _ := h.sim.Lookup(mockserver.AdyenKey(testAccount, cap.Operation.ProviderKey))
	var fx mockserver.AdyenRecord
	json.Unmarshal(rec.Data, &fx)
	failure := notification(eventFields{fx.PSP, fx.PaymentPSP, testAccount, fx.Reference, "100", "USD", "CAPTURE_FAILED", "true"}, true)
	if h.post(failure) != 202 {
		t.Fatal("failure rejected")
	}
	id := h.next()
	// The failure, host review notice and processed_at must share a commit.
	if _, err := db.ExecContext(t.Context(), "ALTER TABLE tender_capture_failures ADD CONSTRAINT fail_review CHECK(false)"); err != nil {
		t.Fatal(err)
	}
	if err := h.hook.Process(t.Context(), id); err == nil {
		t.Fatal("missing rollback")
	}
	current, err := h.e.Get(t.Context(), "pay")
	if err != nil || current.Status != tender.PaymentCaptured {
		t.Fatalf("partial failure commit: %s %v", current.Status, err)
	}
	if _, err := db.ExecContext(t.Context(), "ALTER TABLE tender_capture_failures DROP CONSTRAINT fail_review"); err != nil {
		t.Fatal(err)
	}
	if err := h.hook.Process(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if h.post(failure) != 202 {
		t.Fatal("duplicate rejected")
	}
	h.process()
	db.Close()
	again := open()
	reviews, err := postgres.New(again).CaptureFailures(t.Context(), dbScope)
	if err != nil || len(reviews) != 1 || reviews[0].OperationID != cap.Operation.ID || reviews[0].OperationReference != fx.PSP || reviews[0].Amount != req.Amount {
		t.Fatal(reviews, err)
	}
	var consumed, processed int
	if err := again.QueryRowContext(t.Context(), "SELECT (SELECT COUNT(*) FROM tender_outbox WHERE consumed_at IS NOT NULL),(SELECT COUNT(*) FROM tender_provider_events WHERE event_id=$1 AND processed_at IS NOT NULL)", id).Scan(&consumed, &processed); err != nil || consumed != 1 || processed != 1 {
		t.Fatal(consumed, processed, err)
	}
	if _, err := again.ExecContext(t.Context(), "UPDATE tender_capture_failures SET reviewed_at=CURRENT_TIMESTAMP WHERE operation_id=$1", cap.Operation.ID); err != nil {
		t.Fatal(err)
	}
	reviews, err = postgres.New(again).CaptureFailures(t.Context(), dbScope)
	if err != nil || len(reviews) != 0 {
		t.Fatal(reviews, err)
	}
}

type missingReviewStore struct{ *store.Memory }

func (s missingReviewStore) Transact(ctx context.Context, fn func(tender.Tx) error) error {
	return s.Memory.Transact(ctx, func(tx tender.Tx) error {
		return fn(struct {
			tender.Tx
			tender.VerifiedEvidenceReader
			tender.VerifiedEventTx
		}{tx, tx.(tender.VerifiedEvidenceReader), tx.(tender.VerifiedEventTx)})
	})
}

func TestAdyenContractRequiresFailureQueue(t *testing.T) {
	h := newAdyenRig(t, missingReviewStore{store.NewMemory()}, "Authorised", "")
	h.create()
	if _, err := h.confirm(); err != nil {
		t.Fatal(err)
	}
	h.process()
	cfg := h.cfg
	calls := 0
	cfg.Transport = changeAdyenReply(func(req *http.Request) (*http.Response, error) {
		calls++
		return h.cfg.Transport.RoundTrip(req)
	})
	if _, err := h.engine(cfg, nil).Capture(t.Context(), h.request("cap", 100)); !errors.Is(err, tender.ErrUnsupported) || calls != 0 {
		t.Fatalf("incomplete store dispatched %d capture calls, err %v", calls, err)
	}
}

func (h *adyenRig) record(res tender.Result) mockserver.AdyenRecord {
	h.t.Helper()
	rec, found := h.sim.Lookup(mockserver.AdyenKey(testAccount, res.Operation.ProviderKey))
	if !found {
		h.t.Fatal("missing fixture record")
	}
	var fx mockserver.AdyenRecord
	if err := json.Unmarshal(rec.Data, &fx); err != nil {
		h.t.Fatal(err)
	}
	return fx
}

func TestAdyenObservationRaceHoldDuringModification(t *testing.T) {
	for _, variant := range []string{"capture", "refund", "cancel", "cancel after ack"} {
		t.Run(variant, func(t *testing.T) {
			h := newAdyenRig(t, store.NewMemory(), "Authorised", "")
			h.create()
			auth, err := h.confirm()
			if err != nil {
				t.Fatal(err)
			}
			h.process()
			first, err := h.e.Capture(t.Context(), h.request("first", 40))
			if err != nil {
				t.Fatal(err)
			}
			h.process()
			fx := h.record(first)
			failBody := notification(eventFields{fx.PSP, fx.PaymentPSP, testAccount, fx.Reference, "40", "USD", "CAPTURE_FAILED", "true"}, true)
			hold := func() {
				// Drain the simulator's successful modification notification. The host
				// will receive an original-reference version after the acknowledgement.
				_ = h.next()
				if h.post(failBody) != 202 {
					t.Fatal("hold rejected")
				}
				h.process()
			}
			cfg := h.cfg
			cfg.Transport = changeAdyenReply(func(req *http.Request) (*http.Response, error) {
				resp, err := h.cfg.Transport.RoundTrip(req)
				if err == nil && variant != "cancel after ack" {
					hold()
				}
				return resp, err
			})
			e := h.engine(cfg, nil)
			amount, code, run := int64(60), "CAPTURE", e.Capture
			switch variant {
			case "refund":
				amount, code, run = 40, "REFUND", e.Refund
			case "cancel", "cancel after ack":
				code, run = "CANCELLATION", e.Void
			}
			mod, err := run(t.Context(), h.request("second", amount))
			if err != nil {
				t.Fatal(err)
			}
			second := h.record(mod)
			if mod.Operation.Status != tender.OperationPending || mod.Operation.ProviderReference != second.PSP {
				t.Fatalf("lost modification acknowledgement after hold: %s %q", mod.Operation.Status, mod.Operation.ProviderReference)
			}
			if variant == "cancel after ack" {
				hold()
			}
			// Deliver the original payment's merchant reference. This needs the PSP
			// from the acknowledgement; a modification-reference event would hide a lost binding.
			body := notification(eventFields{second.PSP, second.PaymentPSP, testAccount, h.record(auth).Reference, fmt.Sprint(amount), "USD", code, "true"}, true)
			if h.post(body) != 202 {
				t.Fatal("callback rejected")
			}
			h.process()
			final, err := run(t.Context(), h.request("second", amount))
			if err != nil || final.Payment.Status != tender.PaymentCaptureFailed || final.Operation.Status != tender.OperationSucceeded {
				t.Fatalf("held result: %s operation %s err %v", final.Payment.Status, final.Operation.Status, err)
			}
			captured := int64(40)
			if variant == "capture" {
				captured = 100
			}
			if final.Payment.Funds.Captured != captured || (variant == "refund" && final.Payment.Funds.Refunded != 40) {
				t.Fatal(final.Payment.Funds)
			}
		})
	}
}

func TestAdyenObservationRaceRecoveryOwnership(t *testing.T) {
	runAdyenRecoveryOwnership(t, store.NewMemory())
}

func TestAdyenObservationRaceRecoveryOwnershipSQL(t *testing.T) {
	dsn := os.Getenv("TENDER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("disposable local PostgreSQL required")
	}
	if !adyenLocalDatabase(dsn) {
		t.Fatal("local database required")
	}
	open := postgresFixture(t)
	runAdyenRecoveryOwnership(t, postgres.New(open()))
}

func runAdyenRecoveryOwnership(t *testing.T, db adyenStore) {
	h := newAdyenRig(t, db, "Authorised", "")
	h.create()
	if _, err := h.confirm(); err != nil {
		t.Fatal(err)
	}
	h.process()
	start := time.Now().UTC()
	primary := h.cfg
	started, release := make(chan struct{}), make(chan struct{})
	primary.Transport = changeAdyenReply(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-release
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	req := h.request("first", 40)
	owner := h.engine(primary, func() time.Time { return start })
	done := make(chan error, 1)
	go func() { _, err := owner.Capture(t.Context(), req); done <- err }()
	<-started
	reserved, err := owner.Capture(t.Context(), req)
	if err != nil || reserved.Operation.Status != tender.OperationReserved {
		t.Fatal(reserved.Operation, err)
	}
	t.Run("unexpired owner", func(t *testing.T) {
		cfg := h.cfg
		calls := 0
		cfg.Transport = changeAdyenReply(func(req *http.Request) (*http.Response, error) {
			calls++
			return nil, context.DeadlineExceeded
		})
		if _, err := h.engine(cfg, func() time.Time { return start }).Recover(t.Context(), reserved.Operation.ID); !errors.Is(err, tender.ErrConflict) || calls != 0 {
			t.Errorf("dispatched over live owner: calls=%d err=%v", calls, err)
		}
	})
	recovery := h.cfg
	accepted, releaseAck := make(chan struct{}), make(chan struct{})
	recovery.Transport = changeAdyenReply(func(req *http.Request) (*http.Response, error) {
		resp, err := h.cfg.Transport.RoundTrip(req)
		close(accepted)
		<-releaseAck
		return resp, err
	})
	type outcome struct {
		res tender.Result
		err error
	}
	recovered := make(chan outcome, 1)
	go func() {
		res, err := h.engine(recovery, func() time.Time { return start.Add(time.Minute) }).Recover(t.Context(), reserved.Operation.ID)
		recovered <- outcome{res, err}
	}()
	<-accepted
	event := h.next()
	// The accepted replay has its own durable lease. A third process must
	// leave this key alone even if the old dispatch reports a failure.
	third := h.cfg
	third.Transport = changeAdyenReply(func(*http.Request) (*http.Response, error) {
		t.Error("overlapping recovery dispatch")
		return nil, context.DeadlineExceeded
	})
	if _, err := h.engine(third, func() time.Time { return start.Add(time.Minute) }).Recover(t.Context(), reserved.Operation.ID); !errors.Is(err, tender.ErrConflict) {
		t.Errorf("concurrent replay claim: %v", err)
	}
	close(release)
	primaryErr := <-done
	if errors.Is(primaryErr, tender.ErrRateLimit) {
		t.Error("older 429 closed reservation during accepted replay")
	}
	if _, err := h.e.Capture(t.Context(), h.request("new", 40)); !errors.Is(err, tender.ErrConflict) {
		t.Errorf("fresh capture admitted during replay: %v", err)
	}
	close(releaseAck)
	got := <-recovered
	if got.err != nil || got.res.Operation.Status != tender.OperationPending || got.res.Operation.ProviderReference == "" {
		t.Fatalf("lost accepted replay: %s %v", got.res.Operation.Status, got.err)
	}
	if err := h.hook.Process(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	final, err := h.e.Capture(t.Context(), req)
	if err != nil || final.Payment.Funds.Captured != 40 {
		t.Fatal(final.Payment.Funds, err)
	}
}

func TestAdyenObservationRaceReplayTimeoutAfterWebhook(t *testing.T) {
	h := newAdyenRig(t, store.NewMemory(), "Authorised", "")
	h.create()
	if _, err := h.confirm(); err != nil {
		t.Fatal(err)
	}
	h.process()
	if err := h.sim.SetScenarios("adyen.captures", mockserver.Scenario{Fault: mockserver.LoseResponse}); err != nil {
		t.Fatal(err)
	}
	lost, err := h.e.Capture(t.Context(), h.request("cap", 100))
	if !errors.Is(err, tender.ErrUnknownOutcome) {
		t.Fatal(err)
	}
	cfg := h.cfg
	cfg.Transport = changeAdyenReply(func(req *http.Request) (*http.Response, error) {
		h.process()
		return nil, context.DeadlineExceeded
	})
	got, err := h.engine(cfg, nil).Recover(t.Context(), lost.Operation.ID)
	if err != nil || got.Operation.Status != tender.OperationSucceeded || got.Payment.Funds.Captured != 100 {
		t.Fatalf("stale replay timeout won: %s gross %d error %v", got.Operation.Status, got.Payment.Funds.Captured, err)
	}
}

func TestAdyenContractRecoveryRouteIsolation(t *testing.T) {
	h := newAdyenRig(t, store.NewMemory(), "Authorised", "")
	created := h.create()
	adapter, err := adyen.New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	other, err := tender.NewEngine(tender.EngineConfig{Store: h.db, Scope: dbScope, Market: dbMarket, ProviderName: "adyen", MerchantAccount: "Different", Provider: adapter})
	if err != nil {
		t.Fatal(err)
	}
	got, err := other.Recover(t.Context(), created.Operation.ID)
	if !errors.Is(err, tender.ErrConflict) || got != (tender.Result{}) {
		t.Fatalf("recovery exposed other route %q: %v", got.Payment.ID, err)
	}
}
