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
			o, err := a.Execute(t.Context(), req)
			if err == nil || o.Snapshot.Funds.Captured != 0 {
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
