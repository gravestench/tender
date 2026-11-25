//go:build integration

package contract_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/providers/paypal"
	"github.com/gravestench/tender/store"
	"github.com/gravestench/tender/store/postgres"
	"github.com/gravestench/tender/test/contract"
)

type walletStore interface {
	tender.Store
	Outbox(context.Context, tender.Scope) ([]tender.OutboxEntry, error)
}
type walletRig struct {
	db         walletStore
	cfg        paypal.Config
	engine     *tender.Engine
	hook       *paypal.Webhook
	server     *httptest.Server
	fixture    *mockserver.Server
	clock      atomic.Int64
	tokens     atomic.Int64
	events     chan string
	deliveries chan walletDelivery
}
type walletDelivery struct {
	body   []byte
	header http.Header
}

var walletMarket = tender.Market{Country: "US", Currency: "USD", Method: tender.MethodPayPal}

func newWallet(t *testing.T, db walletStore) *walletRig {
	t.Helper()
	h := &walletRig{db: db, events: make(chan string, 8), deliveries: make(chan walletDelivery, 8)}
	h.clock.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(h.clock.Load(), 0) }
	mux := http.NewServeMux()
	h.server = httptest.NewServer(mux)
	t.Cleanup(h.server.Close)
	var err error
	h.fixture, err = mockserver.New(mockserver.Config{PublicBaseURL: h.server.URL + "/sim", Clock: now, AllowedCallbackTargets: []string{h.server.URL + "/hooks"}, SigningKey: []byte("only-a-fixture-key"), Routes: mockserver.PayPalRoutes(mockserver.PayPalFixture{MerchantAccount: "SYNTHMERCHANT", ClientID: "mockclient", Secret: "mocksecret", WebhookID: "WH-FIXTURE", WebhookURL: h.server.URL + "/hooks"})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.fixture.Close() })
	sim := http.StripPrefix("/sim", h.fixture.Handler())
	mux.HandleFunc("/sim/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sim/v1/oauth2/token" {
			h.tokens.Add(1)
		}
		sim.ServeHTTP(w, r)
	})
	h.cfg = paypal.Config{Scope: dbScope, BaseURL: h.server.URL + "/sim", MerchantAccount: "SYNTHMERCHANT", ClientID: "mockclient", Secret: "mocksecret", WebhookID: "WH-FIXTURE", Now: now}
	a, err := paypal.New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.engine, err = tender.NewEngine(tender.EngineConfig{Store: db, Scope: dbScope, Market: walletMarket, ProviderName: "paypal", MerchantAccount: h.cfg.MerchantAccount, Provider: a, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	h.hook, err = a.Webhook(db, h.engine, func(id string) { h.events <- id })
	if err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("/hooks", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		h.deliveries <- walletDelivery{b, r.Header.Clone()}
		h.hook.ServeHTTP(w, r)
	})
	return h
}
func walletReq(key string, cents int64) tender.Request {
	return tender.Request{PaymentID: "wallet-payment", Key: key, Amount: tender.Money{Currency: "USD", Minor: cents}}
}
func (h *walletRig) create(t *testing.T) tender.Result {
	t.Helper()
	r := walletReq("create", 1000)
	r.OrderID = "order-1"
	res, err := h.engine.Create(t.Context(), r)
	if err != nil || res.Payment.Status != tender.PaymentRequiresAction || res.Payment.Action.Mode != tender.ActionComponent || res.Payment.Action.Data != res.Payment.ProviderReference || res.Payment.ProviderReference == "" {
		t.Fatalf("wallet create = %s %v", res.Payment.Status, err)
	}
	return res
}
func (h *walletRig) decide(t *testing.T, id, decision string) {
	t.Helper()
	res, err := http.Post(h.cfg.BaseURL+"/fixture/orders/"+id+"/"+decision, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 204 {
		t.Fatalf("buyer decision = %d", res.StatusCode)
	}
}
func (h *walletRig) compare(t *testing.T, res tender.Result, route string, amt string) {
	t.Helper()
	err := contract.Verify(t.Context(), h.fixture, h.db, dbScope, res.Operation.ID, mockserver.PayPalKey(h.cfg.MerchantAccount, res.Operation.ProviderKey), func(r contract.Records) error {
		var x mockserver.PayPalRecord
		if json.Unmarshal(r.Fixture.Data, &x) != nil || x.Route != route || x.OrderID != r.Payment.ProviderReference || x.MerchantAccount != r.Payment.MerchantAccount || x.Value != amt || x.Currency != "USD" || route != "create" && x.ResourceID != r.Operation.ProviderReference {
			return fmt.Errorf("host/fixture %s mismatch", route)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func (h *walletRig) event(t *testing.T) string {
	t.Helper()
	select {
	case id := <-h.events:
		return id
	case <-time.After(4 * time.Second):
		t.Fatal("capture webhook missing")
		return ""
	}
}
func TestPayPalContract(t *testing.T) {
	run := func(t *testing.T, db walletStore) {
		h := newWallet(t, db)
		ctx := t.Context()
		res := h.create(t)
		h.compare(t, res, "create", "10.00")
		order := res.Payment.ProviderReference
		// Buyer cancellation never authorizes a debit. Even a forged browser approval must fail.
		h.decide(t, order, "cancel")
		if _, err := h.engine.Confirm(ctx, walletReq("unapproved", 1000)); err == nil {
			t.Fatal("unapproved order collected")
		}
		if p, _ := h.engine.Get(ctx, "wallet-payment"); p.Funds.Captured != 0 {
			t.Fatal("cancelled checkout captured")
		}
		if _, err := h.engine.Capture(ctx, walletReq("capture", 200)); err == nil {
			t.Fatal("standalone capture supported")
		}
		if h.tokens.Load() != 1 {
			t.Fatal("unused token cache")
		}
		h.clock.Add(61)
		h.decide(t, order, "approve")
		res, err := h.engine.Confirm(ctx, walletReq("approved", 1000))
		if err != nil || res.Payment.Funds.Captured != 1000 || res.Payment.VerifiedMethod != tender.MethodPayPal {
			t.Fatalf("capture = %+v %v", res.Payment.Snapshot, err)
		}
		if h.tokens.Load() != 2 {
			t.Fatalf("expected OAuth refresh, calls = %d", h.tokens.Load())
		}
		h.compare(t, res, "capture", "10.00")
		id := h.event(t)
		if err := h.hook.Process(ctx, id); err != nil {
			t.Fatal(err)
		}
		d := <-h.deliveries
		replay := func(body []byte, header http.Header) int {
			r := httptest.NewRequest("POST", "/hooks", bytes.NewReader(body))
			r.Header = header.Clone()
			w := httptest.NewRecorder()
			h.hook.ServeHTTP(w, r)
			return w.Code
		}
		if replay(d.body, d.header) != 202 {
			t.Fatal("verified replay rejected")
		}
		if code := paypalSignedCapture(t, h, id, "PAYMENT.CAPTURE.COMPLETED", order, res.Operation.ProviderReference, h.cfg.MerchantAccount, "9.99"); code == 202 {
			t.Fatal("processed receipt accepted a changed verified amount under the same event ID")
		}
		if err := h.hook.Process(ctx, id); err != nil {
			t.Fatal(err)
		}
		if replay(bytes.Replace(d.body, []byte("10.00"), []byte("99.00"), 1), d.header) == 202 {
			t.Fatal("modified amount acknowledged")
		}
		head := d.header.Clone()
		head.Set("PayPal-Cert-Url", "https://example.invalid/cert")
		if replay(d.body, head) == 202 {
			t.Fatal("foreign certificate accepted")
		}
		res, err = h.engine.Refund(ctx, walletReq("refund", 250))
		if err != nil || res.Payment.Funds.Refunded != 250 {
			t.Fatalf("refund = %+v %v", res.Payment.Funds, err)
		}
		h.compare(t, res, "refund", "2.50")
		again, err := h.engine.Refund(ctx, walletReq("refund", 250))
		if err != nil || again.Operation.ID != res.Operation.ID || again.Payment.Funds.Refunded != 250 {
			t.Fatal("duplicate refund")
		}
		box, err := db.Outbox(ctx, dbScope)
		if err != nil || len(box) != 1 {
			t.Fatal("wallet fulfillment count", len(box), err)
		}
	}
	t.Run("memory", func(t *testing.T) { run(t, store.NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("TENDER_TEST_DATABASE_URL")
		if dsn == "" {
			t.Skip("requires local disposable PostgreSQL")
		}
		if !adyenLocalDatabase(dsn) {
			t.Fatal("local-only SQL required")
		}
		run(t, postgres.New(postgresFixture(t)()))
	})
}
func TestPayPalLostCapture(t *testing.T) {
	for _, viaWebhook := range []bool{true, false} {
		t.Run(fmt.Sprint("webhook_", viaWebhook), func(t *testing.T) {
			h := newWallet(t, store.NewMemory())
			o := h.create(t)
			h.decide(t, o.Payment.ProviderReference, "approve")
			if err := h.fixture.SetScenarios("paypal.capture", mockserver.Scenario{Fault: mockserver.LoseResponse}); err != nil {
				t.Fatal(err)
			}
			res, err := h.engine.Confirm(t.Context(), walletReq("confirm", 1000))
			if !errors.Is(err, tender.ErrUnknownOutcome) || res.Operation.Status != tender.OperationUnknown {
				t.Fatalf("lost capture %s %v", res.Operation.Status, err)
			}
			if _, err = h.engine.Confirm(t.Context(), walletReq("new", 1000)); !errors.Is(err, tender.ErrConflict) {
				t.Fatal("conflicting confirmation")
			}
			id := h.event(t)
			if viaWebhook {
				err = h.hook.Process(t.Context(), id)
			} else {
				_, err = h.engine.Recover(t.Context(), res.Operation.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			p, err := h.engine.Get(t.Context(), "wallet-payment")
			if err != nil || p.Funds.Captured != 1000 || p.VerifiedMethod != tender.MethodPayPal {
				t.Fatalf("recovery = %+v %v", p.Funds, err)
			}
		})
	}
}
func TestPayPalEnvironment(t *testing.T) {
	h := newWallet(t, store.NewMemory())
	for _, url := range []string{"https://api-m.paypal.com", "http://localhost:80", "http://127.0.0.1:80/?x=1", "http://user@127.0.0.1:80"} {
		c := h.cfg
		c.BaseURL = url
		if _, err := paypal.New(c); err == nil {
			t.Errorf("accepted mock host %s", url)
		}
	}
	c := h.cfg
	c.Secret = "bad-secret"
	a, err := paypal.New(c)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := tender.NewEngine(tender.EngineConfig{Provider: a, Store: store.NewMemory(), Scope: dbScope, ProviderName: "paypal", MerchantAccount: c.MerchantAccount, Market: walletMarket})
	r := walletReq("bad", 1000)
	r.OrderID = "order"
	_, err = e.Create(t.Context(), r)
	if !errors.Is(err, tender.ErrAuthentication) || strings.Contains(err.Error(), c.Secret) {
		t.Fatal("invalid credential response", err)
	}
	c.Scope.Environment = tender.EnvironmentSandbox
	c.BaseURL = "https://api-m.sandbox.paypal.com"
	a, err = paypal.New(c)
	if err != nil || a.Capabilities().SupportsIn(tender.OpCreate, walletMarket, false) || len(a.Capabilities().ActionModes) != 0 {
		t.Fatal("ordinary sandbox checkout enabled", err)
	}
	c.Scope.Environment = tender.EnvironmentLive
	c.BaseURL = "https://api-m.paypal.com"
	if _, err := paypal.New(c); err == nil {
		t.Fatal("live without activation")
	}
}

func TestPayPalWireReplay(t *testing.T) {
	h := newWallet(t, store.NewMemory())
	created := h.create(t)
	order := created.Payment.ProviderReference
	h.decide(t, order, "approve")
	res, err := h.engine.Confirm(t.Context(), walletReq("confirm", 1000))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", h.cfg.BaseURL+"/v1/oauth2/token", strings.NewReader("grant_type=client_credentials"))
	req.SetBasicAuth(h.cfg.ClientID, h.cfg.Secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reply, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	var token struct {
		Access string `json:"access_token"`
	}
	if json.NewDecoder(reply.Body).Decode(&token) != nil || token.Access == "" {
		t.Fatal("fixture token")
	}
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("POST", h.cfg.BaseURL+"/v2/checkout/orders/"+order+"/capture", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+token.Access)
		req.Header.Set("PayPal-Request-Id", res.Operation.ProviderKey)
		req.Header.Set("Prefer", "return=representation")
		reply, err := h.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var o struct {
			Units []struct {
				Payments struct{ Captures []struct{ ID string } }
			} `json:"purchase_units"`
		}
		err = json.NewDecoder(reply.Body).Decode(&o)
		reply.Body.Close()
		if err != nil || reply.StatusCode != 200 || len(o.Units) != 1 || len(o.Units[0].Payments.Captures) != 1 || o.Units[0].Payments.Captures[0].ID != res.Operation.ProviderReference {
			t.Fatal("wire replay lost original capture")
		}
	}
}

type walletTransport func(*http.Request) (*http.Response, error)

func (f walletTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestPayPalAuthenticatedOrderBinding(t *testing.T) {
	for _, field := range []string{"payee", "currency", "price", "method", "buyer", "reference", "payment", "captureID", "scope", "key"} {
		t.Run(field, func(t *testing.T) {
			h := newWallet(t, store.NewMemory())
			created := h.create(t)
			h.decide(t, created.Payment.ProviderReference, "approve")
			r := walletReq("confirm", 1000)
			var mutation atomic.Int64
			h.cfg.Transport = walletTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/sim/v2/checkout/orders/"+created.Payment.ProviderReference+"/capture" {
					mutation.Add(1)
				}
				res, err := h.server.Client().Transport.RoundTrip(req)
				if err != nil || req.Method != "GET" {
					return res, err
				}
				var o map[string]any
				body, err := io.ReadAll(res.Body)
				res.Body.Close()
				if err != nil {
					return nil, err
				}
				if err := json.Unmarshal(body, &o); err != nil {
					return nil, err
				}
				u := o["purchase_units"].([]any)[0].(map[string]any)
				switch field {
				case "payee":
					u["payee"].(map[string]any)["merchant_id"] = "ANOTHER"
				case "currency":
					u["amount"].(map[string]any)["currency_code"] = "CAD"
				case "price":
					u["amount"].(map[string]any)["value"] = "9.99"
				case "method":
					o["payment_source"] = map[string]any{"card": map[string]any{}}
				case "buyer":
					o["payment_source"] = map[string]any{"paypal": map[string]any{}}
				case "reference":
					u["reference_id"] = "foreign-attempt"
				case "payment":
					u["custom_id"] = "foreign-payment"
				case "captureID":
					if payments, ok := u["payments"]; ok {
						payments.(map[string]any)["captures"].([]any)[0].(map[string]any)["id"] = ""
					}
				}
				body, _ = json.Marshal(o)
				res.Body = io.NopCloser(bytes.NewReader(body))
				return res, nil
			})
			a, err := paypal.New(h.cfg)
			if err != nil {
				t.Fatal(err)
			}
			req := tender.ProviderRequest{Payment: created.Payment, Operation: tender.Operation{Scope: dbScope, ID: "confirm", PaymentID: created.Payment.ID, AttemptID: created.Payment.AttemptID, Kind: tender.OpConfirm, ProviderKey: "stable", Amount: r.Amount}}
			if field == "scope" {
				req.Payment.Scope.MerchantID = "other"
			}
			if field == "key" {
				req.Operation.ProviderKey = "bad\nkey"
			}
			if field == "captureID" {
				result, err := h.engineWith(t, h.cfg.Transport).Confirm(t.Context(), r)
				if err != nil || result.Operation.Status != tender.OperationPending || result.Payment.Funds.Captured != 0 || mutation.Load() != 1 {
					t.Fatalf("missing capture id escaped response hold: %s %v", result.Operation.Status, err)
				}
				return
			}
			o, err := a.Execute(t.Context(), req)
			if err == nil && (o.Status != tender.OperationPending || o.Snapshot.ProviderStatus != "CaptureEvidenceConflict") || o.Snapshot.Funds.Captured != 0 {
				t.Fatalf("unbound order yielded %d %v", o.Snapshot.Funds.Captured, err)
			}
			if field != "captureID" && mutation.Load() != 0 {
				t.Fatal("unbound order dispatched capture")
			}
		})
	}
}

func TestPayPalUnknownRefund(t *testing.T) {
	h := newWallet(t, store.NewMemory())
	p := h.create(t)
	h.decide(t, p.Payment.ProviderReference, "approve")
	if _, err := h.engine.Confirm(t.Context(), walletReq("confirm", 1000)); err != nil {
		t.Fatal(err)
	}
	if err := h.fixture.SetScenarios("paypal.refund", mockserver.Scenario{Fault: mockserver.LoseResponse}); err != nil {
		t.Fatal(err)
	}
	r := walletReq("refund", 250)
	lost, err := h.engine.Refund(t.Context(), r)
	if !errors.Is(err, tender.ErrUnknownOutcome) || lost.Operation.Status != tender.OperationUnknown || lost.Payment.Funds.Refunded != 0 {
		t.Fatalf("ambiguous refund %s %v", lost.Operation.Status, err)
	}
	for _, key := range []string{"refund", "another"} {
		r.Key = key
		_, err = h.engine.Refund(t.Context(), r)
		if !errors.Is(err, tender.ErrConflict) && !errors.Is(err, tender.ErrUnknownOutcome) {
			t.Fatal("ambiguous refund dispatched again", err)
		}
	}
	if _, err = h.engine.Recover(t.Context(), lost.Operation.ID); !errors.Is(err, tender.ErrUnsupported) {
		t.Fatal("unbound refund was treated as known", err)
	}
}
func TestPayPalBearerChallenge(t *testing.T) {
	h := newWallet(t, store.NewMemory())
	var first atomic.Bool
	var calls atomic.Int64
	h.cfg.Transport = walletTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/sim/v2/checkout/orders" && !first.Swap(true) {
			return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		}
		if r.URL.Path == "/sim/v2/checkout/orders" {
			calls.Add(1)
		}
		return h.server.Client().Transport.RoundTrip(r)
	})
	a, err := paypal.New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.engine, err = tender.NewEngine(tender.EngineConfig{Store: h.db, Provider: a, Scope: dbScope, Market: walletMarket, ProviderName: "paypal", MerchantAccount: h.cfg.MerchantAccount})
	if err != nil {
		t.Fatal(err)
	}
	h.create(t)
	if !first.Load() || calls.Load() != 1 || h.tokens.Load() != 2 {
		t.Fatal("failed to refresh a rejected token exactly once")
	}
}

func (h *walletRig) engineWith(t *testing.T, transport http.RoundTripper) *tender.Engine {
	t.Helper()
	c := h.cfg
	c.Transport = transport
	a, err := paypal.New(c)
	if err != nil {
		t.Fatal(err)
	}
	e, err := tender.NewEngine(tender.EngineConfig{Provider: a, Store: h.db, Scope: dbScope, Market: walletMarket, ProviderName: "paypal", MerchantAccount: c.MerchantAccount})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func TestPayPalNoDispatchRetry(t *testing.T) {
	for _, kind := range []string{"create", "confirm", "refund"} {
		t.Run(kind, func(t *testing.T) {
			h := newWallet(t, store.NewMemory())
			ctx := t.Context()
			if kind != "create" {
				p := h.create(t)
				h.decide(t, p.Payment.ProviderReference, "approve")
				if kind == "refund" {
					if _, err := h.engine.Confirm(ctx, walletReq("confirm", 1000)); err != nil {
						t.Fatal(err)
					}
				}
			}
			target := "/sim/v1/oauth2/token"
			if kind != "create" {
				target = "/sim/v2/checkout/orders/"
			}
			var failed atomic.Bool
			var sent atomic.Int64
			e := h.engineWith(t, walletTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasPrefix(r.URL.Path, target) && !failed.Swap(true) {
					return nil, io.ErrUnexpectedEOF
				}
				if r.Header.Get("PayPal-Request-Id") != "" {
					sent.Add(1)
				}
				return h.server.Client().Transport.RoundTrip(r)
			}))
			r := walletReq("first", 1000)
			var op func(context.Context, tender.Request) (tender.Result, error)
			switch kind {
			case "create":
				op = e.Create
				r.OrderID = "order-1"
			case "confirm":
				op = e.Confirm
			case "refund":
				op = e.Refund
				r.Amount.Minor = 250
			}
			lost, err := op(ctx, r)
			if !errors.Is(err, tender.ErrNotDispatched) || lost.Operation.Status != tender.OperationFailed || sent.Load() != 0 {
				t.Fatalf("preflight %s = %s %v; mutation sends %d", kind, lost.Operation.Status, err, sent.Load())
			}
			r.Key = "retry"
			if kind == "create" {
				_, err = e.RetryCreate(ctx, lost.Operation.ID, r)
			} else {
				_, err = op(ctx, r)
			}
			if err != nil || sent.Load() != 1 {
				t.Fatalf("safe retry = %v; sends %d", err, sent.Load())
			}
		})
	}
}
func TestPayPalDefinitiveCaptureRejection(t *testing.T) {
	for _, tc := range []struct {
		issue, name string
		code        int
		want        tender.ErrorKind
	}{{"INSTRUMENT_DECLINED", "UNPROCESSABLE_ENTITY", 422, tender.ErrDeclined}, {"INVALID_PARAMETER_VALUE", "INVALID_REQUEST", 400, tender.ErrValidation}, {"ORDER_ALREADY_CAPTURED", "UNPROCESSABLE_ENTITY", 422, tender.ErrUnknownOutcome}, {"NEW_ISSUE", "UNPROCESSABLE_ENTITY", 422, tender.ErrUnknownOutcome}} {
		t.Run(tc.issue, func(t *testing.T) {
			h := newWallet(t, store.NewMemory())
			ctx := t.Context()
			p := h.create(t)
			h.decide(t, p.Payment.ProviderReference, "approve")
			var first atomic.Bool
			e := h.engineWith(t, walletTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/capture") && !first.Swap(true) {
					return &http.Response{StatusCode: tc.code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"name":%q,"details":[{"issue":%q,"description":"private text"}]}`, tc.name, tc.issue)))}, nil
				}
				return h.server.Client().Transport.RoundTrip(r)
			}))
			res, err := e.Confirm(ctx, walletReq("first", 1000))
			if !errors.Is(err, tc.want) || res.Payment.Funds.Captured != 0 {
				t.Fatalf("rejection = %s %v", res.Operation.Status, err)
			}
			next, err := e.Confirm(ctx, walletReq("second", 1000))
			if tc.want == tender.ErrUnknownOutcome {
				if !errors.Is(err, tender.ErrConflict) {
					t.Fatal("unclassified error released reservation")
				}
				return
			}
			if err != nil || next.Payment.Funds.Captured != 1000 {
				t.Fatal("definitive rejection prevented new attempt", err)
			}
		})
	}
}
func TestPayPalContradictoryCaptureEvidence(t *testing.T) {
	for _, tc := range []string{"post capture id", "get capture id", "post buyer", "get buyer"} {
		t.Run(tc, func(t *testing.T) {
			h := newWallet(t, store.NewMemory())
			ctx := t.Context()
			p := h.create(t)
			h.decide(t, p.Payment.ProviderReference, "approve")
			var posted atomic.Bool
			tr := walletTransport(func(r *http.Request) (*http.Response, error) {
				res, err := h.server.Client().Transport.RoundTrip(r)
				if err != nil {
					return res, err
				}
				post := strings.HasSuffix(r.URL.Path, "/capture")
				if post {
					posted.Store(true)
				}
				if !posted.Load() || post != strings.HasPrefix(tc, "post") || !post && r.Method != "GET" {
					return res, nil
				}
				var obj map[string]any
				err = json.NewDecoder(res.Body).Decode(&obj)
				res.Body.Close()
				if err != nil {
					return nil, err
				}
				if strings.HasSuffix(tc, "buyer") {
					obj["payment_source"].(map[string]any)["paypal"].(map[string]any)["account_id"] = "OTHERBUYER"
				} else {
					obj["purchase_units"].([]any)[0].(map[string]any)["payments"].(map[string]any)["captures"].([]any)[0].(map[string]any)["id"] = "C2WRONG"
				}
				raw, _ := json.Marshal(obj)
				res.Body = io.NopCloser(bytes.NewReader(raw))
				return res, nil
			})
			e := h.engineWith(t, tr)
			res, err := e.Confirm(ctx, walletReq("capture", 1000))
			if res.Payment.Funds.Captured != 0 || res.Operation.Status != tender.OperationPending {
				t.Fatalf("contradictory evidence = %s %d %v", res.Operation.Status, res.Payment.Funds.Captured, err)
			}
			_, err = e.Recover(ctx, res.Operation.ID)
			if !errors.Is(err, tender.ErrUnknownOutcome) {
				t.Fatal("a single disputed query authorized capture", err)
			}
			id := h.event(t)
			if strings.HasPrefix(tc, "get") {
				cfg := h.cfg
				cfg.Transport = tr
				adapter, err := paypal.New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				hook, err := adapter.Webhook(h.db, e, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err = hook.Process(ctx, id); !errors.Is(err, tender.ErrConflict) {
					t.Fatal("genuine signature settled against a conflicting buyer or capture", err)
				}
				p, err := e.Get(ctx, p.Payment.ID)
				if err != nil || p.Funds.Captured != 0 {
					t.Fatal("conflicting webhook read credited funds")
				}
			}
			if err = h.hook.Process(ctx, id); err != nil {
				t.Fatal("verified event couldn't recover real capture", err)
			}
			paid, _ := e.Get(ctx, p.Payment.ID)
			if paid.Funds.Captured != 1000 {
				t.Fatal("no verified capture")
			}
		})
	}
}
func TestPayPalFixtureCurrentStatus(t *testing.T) {
	h := newWallet(t, store.NewMemory())
	ctx := t.Context()
	var originalCreate []byte
	h.engine = h.engineWith(t, walletTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == "POST" && req.URL.Path == "/sim/v2/checkout/orders" {
			var err error
			originalCreate, err = io.ReadAll(req.Body)
			req.Body.Close()
			if err != nil {
				return nil, err
			}
			req.Body = io.NopCloser(bytes.NewReader(originalCreate))
		}
		return h.server.Client().Transport.RoundTrip(req)
	}))
	p := h.create(t)
	h.decide(t, p.Payment.ProviderReference, "approve")
	// Exercise the fixture's API directly with the same access and request IDs.
	req, _ := http.NewRequest("POST", h.cfg.BaseURL+"/v1/oauth2/token", strings.NewReader("grant_type=client_credentials"))
	req.SetBasicAuth(h.cfg.ClientID, h.cfg.Secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var tok struct {
		Token string `json:"access_token"`
	}
	err = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	check := func(orig tender.Result, route, orderStatus, capStatus string) {
		body := []byte("{}")
		if orig.Operation.Kind == tender.OpCreate {
			body = originalCreate
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", h.cfg.BaseURL+route, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		req.Header.Set("PayPal-Request-Id", orig.Operation.ProviderKey)
		req.Header.Set("Prefer", "return=representation")
		resp, err := h.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var o struct {
			Status string
			Units  []struct {
				Payments struct{ Captures []struct{ Status string } }
			} `json:"purchase_units"`
		}
		err = json.NewDecoder(resp.Body).Decode(&o)
		if err != nil || o.Status != orderStatus || capStatus != "" && (len(o.Units) != 1 || len(o.Units[0].Payments.Captures) != 1 || o.Units[0].Payments.Captures[0].Status != capStatus) {
			t.Errorf("replay %s = %s; wanted %s/%s", route, o.Status, orderStatus, capStatus)
		}
	}
	path := "/v2/checkout/orders"
	check(p, path, "APPROVED", "")
	c, err := h.engine.Confirm(ctx, walletReq("confirm", 1000))
	if err != nil {
		t.Fatal(err)
	}
	check(p, path, "COMPLETED", "COMPLETED")
	_, err = h.engine.Refund(ctx, walletReq("refund", 200))
	if err != nil {
		t.Fatal(err)
	}
	check(c, path+"/"+p.Payment.ProviderReference+"/capture", "COMPLETED", "PARTIALLY_REFUNDED")
}
func TestPayPalBuyerResume(t *testing.T) {
	h := newWallet(t, store.NewMemory())
	p := h.create(t)
	h.decide(t, p.Payment.ProviderReference, "approve")
	// The provider approved, then the buyer disconnected before host Confirm.
	h.decide(t, p.Payment.ProviderReference, "approve")
	r := walletReq("confirm", 1000)
	res, err := h.engine.Confirm(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := h.engine.Confirm(t.Context(), r)
	if err != nil || repeat.Operation.ProviderReference != res.Operation.ProviderReference || repeat.Payment.Funds.Captured != 1000 {
		t.Fatal("resumed confirmation was not idempotent")
	}
}

func TestPayPalMutationErrorScope(t *testing.T) {
	for _, tc := range []struct {
		kind, issue string
		code        int
		want        tender.ErrorKind
	}{{"create", "MISSING_REQUIRED_PARAMETER", 400, tender.ErrValidation}, {"refund", "REFUND_AMOUNT_EXCEEDED", 422, tender.ErrValidation}, {"refund", "INSTRUMENT_DECLINED", 422, tender.ErrUnknownOutcome}, {"capture", "REFUND_AMOUNT_EXCEEDED", 422, tender.ErrUnknownOutcome}, {"capture", "oversize", 400, tender.ErrUnknownOutcome}} {
		t.Run(tc.kind+"_"+tc.issue, func(t *testing.T) {
			h := newWallet(t, store.NewMemory())
			ctx := t.Context()
			r := walletReq("error", 1000)
			if tc.kind != "create" {
				p := h.create(t)
				h.decide(t, p.Payment.ProviderReference, "approve")
				if tc.kind == "refund" {
					if _, err := h.engine.Confirm(ctx, walletReq("confirm", 1000)); err != nil {
						t.Fatal(err)
					}
					r.Amount.Minor = 250
				}
			} else {
				r.OrderID = "order"
			}
			e := h.engineWith(t, walletTransport(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("PayPal-Request-Id") == "" {
					return h.server.Client().Transport.RoundTrip(req)
				}
				issue := tc.issue
				message := "sensitive-message"
				if issue == "oversize" {
					issue = "INVALID_PARAMETER_VALUE"
					message = strings.Repeat("X", 20000)
				}
				name := "UNPROCESSABLE_ENTITY"
				if tc.code == 400 {
					name = "INVALID_REQUEST"
				}
				raw, _ := json.Marshal(map[string]any{"name": name, "message": message, "details": []map[string]string{{"issue": issue}}})
				return &http.Response{StatusCode: tc.code, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}, nil
			}))
			call := e.Confirm
			if tc.kind == "refund" {
				call = e.Refund
			} else if tc.kind == "create" {
				call = e.Create
			}
			out, err := call(ctx, r)
			if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("endpoint error = %s %v", out.Operation.Status, err)
			}
		})
	}
}

func TestPayPalCallbackWaitsForCaptureComparison(t *testing.T) {
	h := newWallet(t, store.NewMemory())
	ctx := t.Context()
	p := h.create(t)
	h.decide(t, p.Payment.ProviderReference, "approve")
	pending := make(chan struct{})
	release := make(chan struct{})
	e := h.engineWith(t, walletTransport(func(r *http.Request) (*http.Response, error) {
		res, err := h.server.Client().Transport.RoundTrip(r)
		if err == nil && strings.HasSuffix(r.URL.Path, "/capture") {
			close(pending)
			select {
			case <-release:
			case <-r.Context().Done():
				res.Body.Close()
				return nil, r.Context().Err()
			}
		}
		return res, err
	}))
	done := make(chan error, 1)
	go func() { _, err := e.Confirm(ctx, walletReq("confirm", 1000)); done <- err }()
	<-pending
	var op tender.OperationRecord
	if err := h.db.Transact(ctx, func(tx tender.Tx) error { var err error; op, err = tx.OperationByKey(dbScope, "confirm"); return err }); err != nil {
		close(release)
		t.Fatal(err)
	}
	_, earlyQuery := e.Recover(ctx, op.ID)
	id := h.event(t)
	early := h.hook.Process(ctx, id)
	before, err := e.Get(ctx, p.Payment.ID)
	close(release)
	confirmed := <-done
	if !errors.Is(earlyQuery, tender.ErrConflict) {
		t.Fatalf("concurrent recovery bypassed response comparison: %v", earlyQuery)
	}
	if !errors.Is(early, tender.ErrConflict) || err != nil || before.Funds.Captured != 0 || confirmed != nil {
		t.Fatalf("early callback %v, gross %d, confirm %v", early, before.Funds.Captured, confirmed)
	}
	if err := h.hook.Process(ctx, id); err != nil {
		t.Fatal("stored event failed after response", err)
	}
}

func TestPayPalAbandonedReservation(t *testing.T) {
	for _, source := range []string{"query", "webhook", "legacy-webhook"} {
		t.Run(source, func(t *testing.T) {
			h := newWallet(t, store.NewMemory())
			ctx := t.Context()
			created := h.create(t)
			h.decide(t, created.Payment.ProviderReference, "approve")
			real, err := h.engine.Confirm(ctx, walletReq("confirm", 1000))
			if err != nil {
				t.Fatal(err)
			}
			id := h.event(t)
			delivery := <-h.deliveries
			// Restore exactly the durable state left by a worker that died after
			// PayPal captured the order but before the HTTP reply was committed.
			db := store.NewMemory()
			p := created.Payment
			op := real.Operation
			op.Status = tender.OperationReserved
			op.ProviderReference = ""
			until := h.cfg.Now().Add(10 * time.Second)
			lease := until
			if source == "legacy-webhook" {
				op.ProviderEvidenceDigest = [32]byte{}
				lease = time.Time{}
			}
			if err = db.Transact(ctx, func(tx tender.Tx) error {
				if err := tx.InsertPayment(p); err != nil {
					return err
				}
				return tx.InsertOperation(tender.OperationRecord{Operation: op, PaymentVersion: p.Version, DispatchUntil: lease})
			}); err != nil {
				t.Fatal(err)
			}
			adapter, err := paypal.New(h.cfg)
			if err != nil {
				t.Fatal(err)
			}
			e, err := tender.NewEngine(tender.EngineConfig{Store: db, Provider: adapter, Scope: p.Scope, ProviderName: p.Provider, Market: p.Market, MerchantAccount: p.MerchantAccount, Now: h.cfg.Now})
			if err != nil {
				t.Fatal(err)
			}
			hook, err := adapter.Webhook(db, e, nil)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/hooks", bytes.NewReader(delivery.body))
			req.Header = delivery.header
			reply := httptest.NewRecorder()
			hook.ServeHTTP(reply, req)
			if reply.Code != 202 {
				t.Fatal("verified receipt missing")
			}
			recover := func() error {
				if source != "query" {
					return hook.Process(ctx, id)
				}
				_, err := e.Recover(ctx, op.ID)
				return err
			}
			err = recover()
			if source == "legacy-webhook" && err != nil {
				t.Fatal("signed legacy capture blocked by missing lease", err)
			}
			if source != "legacy-webhook" && !errors.Is(err, tender.ErrConflict) {
				t.Fatal("active dispatch fence ignored", err)
			}
			h.clock.Store(until.Unix())
			err = recover()
			if source == "query" {
				if !errors.Is(err, tender.ErrUnknownOutcome) {
					t.Fatal("expired reservation credited an uncorroborated GET", err)
				}
				err = hook.Process(ctx, id)
			}
			if err != nil {
				t.Fatal("abandoned reservation still blocked at expiry", err)
			}
			paid, err := e.Get(ctx, p.ID)
			if err != nil || paid.Funds.Captured != p.Amount.Minor {
				t.Fatal("recovered payment not collected", err)
			}
		})
	}
}

func TestPayPalRoundTwoRejections(t *testing.T) {
	for _, tc := range []struct {
		kind, name string
		code       int
		issues     []string
		want       tender.ErrorKind
	}{
		{"create", "NOT_AUTHORIZED", 403, []string{"PERMISSION_DENIED"}, tender.ErrAuthentication},
		{"capture", "NOT_AUTHORIZED", 403, nil, tender.ErrAuthentication},
		{"refund", "NOT_AUTHORIZED", 403, []string{"PERMISSION_DENIED"}, tender.ErrAuthentication},
		{"refund", "UNPROCESSABLE_ENTITY", 422, []string{"TRANSACTION_DISPUTED"}, tender.ErrValidation},
		{"capture", "UNPROCESSABLE_ENTITY", 422, []string{"TRANSACTION_DISPUTED"}, tender.ErrUnknownOutcome},
		{"refund", "NOT_AUTHORIZED", 403, []string{"PERMISSION_DENIED", "SOME_NEW_ISSUE"}, tender.ErrUnknownOutcome},
	} {
		t.Run(tc.kind+"_"+tc.name+"_"+strings.Join(tc.issues, "_"), func(t *testing.T) {
			h := newWallet(t, store.NewMemory())
			ctx := t.Context()
			r := walletReq("reject", 1000)
			if tc.kind == "create" {
				r.OrderID = "order"
			} else {
				p := h.create(t)
				h.decide(t, p.Payment.ProviderReference, "approve")
				if tc.kind == "refund" {
					if _, err := h.engine.Confirm(ctx, walletReq("confirm", 1000)); err != nil {
						t.Fatal(err)
					}
					r.Amount.Minor = 200
				}
			}
			var once atomic.Bool
			e := h.engineWith(t, walletTransport(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("PayPal-Request-Id") == "" || once.Swap(true) {
					return h.server.Client().Transport.RoundTrip(req)
				}
				var details []map[string]string
				for _, issue := range tc.issues {
					details = append(details, map[string]string{"issue": issue})
				}
				raw, _ := json.Marshal(map[string]any{"name": tc.name, "details": details})
				return &http.Response{StatusCode: tc.code, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}, nil
			}))
			call := e.Confirm
			if tc.kind == "create" {
				call = e.Create
			} else if tc.kind == "refund" {
				call = e.Refund
			}
			res, err := call(ctx, r)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %s %v", res.Operation.Status, err)
			}
			if tc.want == tender.ErrUnknownOutcome {
				return
			}
			r.Key = "retry"
			if tc.kind == "create" {
				_, err = e.RetryCreate(ctx, res.Operation.ID, r)
			} else {
				_, err = call(ctx, r)
			}
			if err != nil {
				t.Fatal("safe retry after rejection", err)
			}
		})
	}
}
func TestPayPalElapsedBeforeExecute(t *testing.T) {
	h := newWallet(t, store.NewMemory())
	adapter, err := paypal.New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var reads atomic.Int64
	e, err := tender.NewEngine(tender.EngineConfig{Store: h.db, Provider: adapter, Scope: dbScope, Market: walletMarket, ProviderName: "paypal", MerchantAccount: h.cfg.MerchantAccount, Timeout: time.Second, Now: func() time.Time {
		if reads.Add(1) > 2 {
			return start.Add(3 * time.Second)
		}
		return start
	}})
	if err != nil {
		t.Fatal(err)
	}
	r := walletReq("create", 1000)
	r.OrderID = "order"
	res, err := e.Create(t.Context(), r)
	if h.tokens.Load() != 0 || res.Operation.Status != tender.OperationFailed || !errors.Is(err, tender.ErrNotDispatched) {
		t.Fatalf("deadline before Execute = %s %v, token requests %d", res.Operation.Status, err, h.tokens.Load())
	}
	r.Key = "next"
	if _, err = e.RetryCreate(t.Context(), res.Operation.ID, r); err != nil {
		t.Fatal("safe retry failed", err)
	}
}
func TestPayPalLostBuyerBinding(t *testing.T) {
	for _, fault := range []mockserver.Fault{mockserver.LoseResponse, mockserver.MalformedResponse} {
		for _, viaWebhook := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_webhook_%t", fault, viaWebhook), func(t *testing.T) { runLostBuyerBinding(t, store.NewMemory(), fault, viaWebhook) })
		}
	}
}
func TestPayPalLostBuyerBindingSQL(t *testing.T) {
	dsn := os.Getenv("TENDER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	if !adyenLocalDatabase(dsn) {
		t.Fatal("local-only SQL required")
	}
	for _, viaWebhook := range []bool{false, true} {
		t.Run(fmt.Sprintf("webhook_%t", viaWebhook), func(t *testing.T) {
			runLostBuyerBinding(t, postgres.New(postgresFixture(t)()), mockserver.LoseResponse, viaWebhook)
		})
	}
}
func runLostBuyerBinding(t *testing.T, db walletStore, fault mockserver.Fault, byHook bool) {
	h := newWallet(t, db)
	ctx := t.Context()
	created := h.create(t)
	h.decide(t, created.Payment.ProviderReference, "approve")
	if err := h.fixture.SetScenarios("paypal.capture", mockserver.Scenario{Fault: fault}); err != nil {
		t.Fatal(err)
	}
	var switched atomic.Bool
	var stored atomic.Bool
	tr := walletTransport(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/capture") {
			var exists bool
			err := h.db.Transact(ctx, func(tx tender.Tx) error {
				op, err := tx.OperationByKey(dbScope, "confirm")
				if err != nil {
					return err
				}
				exists = op.ProviderEvidenceDigest != ([32]byte{})
				return nil
			})
			if err != nil {
				return nil, err
			}
			stored.Store(exists)
		}
		res, err := h.server.Client().Transport.RoundTrip(req)
		if err != nil || !switched.Load() || req.Method != "GET" {
			return res, err
		}
		var o map[string]any
		err = json.NewDecoder(res.Body).Decode(&o)
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		o["payment_source"].(map[string]any)["paypal"].(map[string]any)["account_id"] = "BUYER_B"
		raw, _ := json.Marshal(o)
		res.Body = io.NopCloser(bytes.NewReader(raw))
		return res, nil
	})
	e := h.engineWith(t, tr)
	lost, err := e.Confirm(ctx, walletReq("confirm", 1000))
	if !errors.Is(err, tender.ErrUnknownOutcome) {
		t.Fatal(err)
	}
	id := h.event(t)
	switched.Store(true)
	restarted := h.engineWith(t, tr)
	cfg := h.cfg
	cfg.Transport = tr
	a, err := paypal.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hook, err := a.Webhook(h.db, restarted, nil)
	if err != nil {
		t.Fatal(err)
	}
	recover := func() error {
		if byHook {
			return hook.Process(ctx, id)
		}
		_, err := restarted.Recover(ctx, lost.Operation.ID)
		return err
	}
	err = recover()
	p, readErr := restarted.Get(ctx, created.Payment.ID)
	if !stored.Load() || err == nil || readErr != nil || p.Funds.Captured != 0 {
		t.Fatalf("pre-POST binding %t; wrong buyer got %d, %v", stored.Load(), p.Funds.Captured, err)
	}
	switched.Store(false)
	if err = recover(); err != nil {
		t.Fatal("original buyer recovery", err)
	}
	p, _ = restarted.Get(ctx, p.ID)
	if p.Funds.Captured != 1000 {
		t.Fatal("correct buyer missing capture")
	}
}
func TestPayPalLiveFinisherAfterExpiry(t *testing.T) {
	h := newWallet(t, store.NewMemory())
	ctx := t.Context()
	p := h.create(t)
	h.decide(t, p.Payment.ProviderReference, "approve")
	held := make(chan struct{})
	release := make(chan struct{})
	cfg := h.cfg
	cfg.Transport = walletTransport(func(r *http.Request) (*http.Response, error) {
		res, err := h.server.Client().Transport.RoundTrip(r)
		if err != nil || !strings.HasSuffix(r.URL.Path, "/capture") {
			return res, err
		}
		var o map[string]any
		err = json.NewDecoder(res.Body).Decode(&o)
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		o["purchase_units"].([]any)[0].(map[string]any)["payments"].(map[string]any)["captures"].([]any)[0].(map[string]any)["id"] = "CONTRADICTORY"
		b, _ := json.Marshal(o)
		res.Body = io.NopCloser(bytes.NewReader(b))
		close(held)
		<-release
		return res, nil
	})
	a, err := paypal.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e, err := tender.NewEngine(tender.EngineConfig{Store: h.db, Provider: a, Scope: dbScope, Market: walletMarket, ProviderName: "paypal", MerchantAccount: cfg.MerchantAccount, Now: cfg.Now, Timeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := e.Confirm(ctx, walletReq("capture", 1000)); done <- err }()
	<-held
	var op tender.OperationRecord
	if err := h.db.Transact(ctx, func(tx tender.Tx) error { var err error; op, err = tx.OperationByKey(dbScope, "capture"); return err }); err != nil {
		close(release)
		t.Fatal(err)
	}
	h.clock.Store(op.DispatchUntil.Add(time.Second).Unix())
	_, recovery := e.Recover(ctx, op.ID)
	close(release)
	<-done
	current, _ := e.Get(ctx, p.Payment.ID)
	if !errors.Is(recovery, tender.ErrUnknownOutcome) || current.Funds.Captured != 0 || current.ProviderStatus != "CaptureEvidenceConflict" {
		t.Fatalf("expired deadline supplanted live comparison = %v %s %d", recovery, current.ProviderStatus, current.Funds.Captured)
	}
	if err = h.hook.Process(ctx, h.event(t)); err != nil {
		t.Fatal("signed corroboration did not reconcile", err)
	}
}
func TestPayPalCheckpointRollbackSQL(t *testing.T) {
	dsn := os.Getenv("TENDER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	if !adyenLocalDatabase(dsn) {
		t.Fatal("local-only SQL required")
	}
	sqlDB := postgresFixture(t)()
	h := newWallet(t, postgres.New(sqlDB))
	ctx := t.Context()
	p := h.create(t)
	h.decide(t, p.Payment.ProviderReference, "approve")
	if _, err := sqlDB.ExecContext(ctx, `ALTER TABLE tender_operations ADD CONSTRAINT reject_paypal_binding CHECK (NOT (record ? 'ProviderEvidenceDigest'))`); err != nil {
		t.Fatal(err)
	}
	res, err := h.engine.Confirm(ctx, walletReq("fail", 1000))
	if !errors.Is(err, tender.ErrNotDispatched) || res.Operation.Status != tender.OperationFailed || res.Operation.ProviderEvidenceDigest != ([32]byte{}) {
		t.Fatalf("failed checkpoint %s %v", res.Operation.Status, err)
	}
	if _, found := h.fixture.Lookup(mockserver.PayPalKey(h.cfg.MerchantAccount, res.Operation.ProviderKey)); found {
		t.Fatal("capture dispatched without its SQL checkpoint")
	}
	if _, err = sqlDB.ExecContext(ctx, `ALTER TABLE tender_operations DROP CONSTRAINT reject_paypal_binding`); err != nil {
		t.Fatal(err)
	}
	res, err = h.engine.Confirm(ctx, walletReq("next", 1000))
	if err != nil || res.Payment.Funds.Captured != 1000 {
		t.Fatal("could not resume after checkpoint failure", err)
	}
	err = h.db.Transact(ctx, func(tx tender.Tx) error {
		record, err := tx.Operation(dbScope, res.Operation.ID)
		if err != nil {
			return err
		}
		record.ProviderEvidenceDigest = [32]byte{}
		return tx.UpdateOperation(record)
	})
	if !errors.Is(err, tender.ErrConflict) {
		t.Fatal("could clear durable buyer binding", err)
	}
}
func TestPayPalAbandonedBeforeBuyerCheckpoint(t *testing.T) {
	h := newWallet(t, store.NewMemory())
	ctx := t.Context()
	p := h.create(t)
	h.decide(t, p.Payment.ProviderReference, "approve")
	held := make(chan struct{})
	release := make(chan struct{})
	var first atomic.Bool
	var sends atomic.Int64
	cfg := h.cfg
	cfg.Transport = walletTransport(func(req *http.Request) (*http.Response, error) {
		res, err := h.server.Client().Transport.RoundTrip(req)
		if req.Header.Get("PayPal-Request-Id") != "" {
			sends.Add(1)
		}
		if err == nil && req.Method == "GET" && !first.Swap(true) {
			close(held)
			<-release
		}
		return res, err
	})
	a, err := paypal.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e, err := tender.NewEngine(tender.EngineConfig{Store: h.db, Provider: a, Scope: dbScope, Market: walletMarket, ProviderName: "paypal", MerchantAccount: cfg.MerchantAccount, Now: cfg.Now})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := e.Confirm(ctx, walletReq("stopped", 1000)); done <- err }()
	<-held
	var op tender.OperationRecord
	if err = h.db.Transact(ctx, func(tx tender.Tx) error { var err error; op, err = tx.OperationByKey(dbScope, "stopped"); return err }); err != nil {
		close(release)
		t.Fatal(err)
	}
	h.clock.Store(op.DispatchUntil.Add(time.Second).Unix())
	res, preflight := e.Recover(ctx, op.ID)
	close(release)
	late := <-done
	if !errors.Is(preflight, tender.ErrNotDispatched) || res.Operation.Status != tender.OperationFailed || !errors.Is(late, tender.ErrNotDispatched) || sends.Load() != 0 {
		t.Fatalf("abandoned preflight %s %v late %v sends %d", res.Operation.Status, preflight, late, sends.Load())
	}
	if _, err = e.Confirm(ctx, walletReq("next", 1000)); err != nil || sends.Load() != 1 {
		t.Fatal("could not safely resume abandoned preflight", err)
	}
}
