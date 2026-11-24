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
	"strings"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/providers/klarna"
	"github.com/gravestench/tender/store"
	"github.com/gravestench/tender/store/postgres"
	"github.com/gravestench/tender/test/contract"
)

type capturedKlarnaCallback struct{ target, token string }

type klarnaRig struct {
	seen chan capturedKlarnaCallback
	t    *testing.T
	e    *tender.Engine
	a    *klarna.Adapter
	db   adyenStore
	cfg  klarna.Config
	sim  *mockserver.Server
	url  string
	now  time.Time
}

func newKlarna(t *testing.T, db adyenStore, fx mockserver.KlarnaFixture) *klarnaRig {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := &klarnaRig{seen: make(chan capturedKlarnaCallback, 1), t: t, db: db, url: srv.URL, now: time.Now().UTC()}
	fx.MerchantAccount = "mid"
	fx.APIKey = "mock-key"
	fx.CallbackURL = srv.URL + "/callback"
	var err error
	h.sim, err = mockserver.New(mockserver.Config{PublicBaseURL: srv.URL + "/sim", Routes: mockserver.KlarnaRoutes(fx)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.sim.Close() })
	mux.Handle("/sim/", http.StripPrefix("/sim", h.sim.Handler()))
	h.cfg = klarna.Config{Scope: dbScope, Store: db, BaseURL: srv.URL + "/sim", APIKey: "mock-key", MerchantAccount: "mid", CallbackURL: fx.CallbackURL, CallbackKey: []byte("0123456789abcdef0123456789abcdef"), Now: func() time.Time { return h.now }, OrderLines: func(context.Context, tender.Payment) ([]klarna.Line, error) {
		return []klarna.Line{{Type: "digital", Name: "Course", Reference: "course", Quantity: 1, UnitPrice: 100, TotalAmount: 100}}, nil
	}}
	h.restart()
	mux.Handle("/callback", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var payload struct {
			Token string `json:"authorization_token"`
		}
		_ = json.Unmarshal(raw, &payload)
		h.a.CallbackHandler().ServeHTTP(w, r)
		select {
		case h.seen <- capturedKlarnaCallback{target: h.url + r.URL.RequestURI(), token: payload.Token}:
		default:
		}
	}))
	return h
}
func (h *klarnaRig) restart() {
	h.t.Helper()
	var err error
	h.a, err = klarna.New(h.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	h.e, err = tender.NewEngine(tender.EngineConfig{Scope: dbScope, Store: h.db, Provider: h.a, ProviderName: "klarna", MerchantAccount: "mid", Market: tender.Market{Country: "US", Currency: "USD", Method: tender.MethodKlarna}, Now: h.cfg.Now})
	if err != nil {
		h.t.Fatal(err)
	}
}
func kreq(key string, amount int64) tender.Request {
	return tender.Request{PaymentID: "pay", Key: key, Amount: tender.Money{Currency: "USD", Minor: amount}}
}
func (h *klarnaRig) create() (tender.Result, error) {
	r := kreq("create", 100)
	r.OrderID = "order"
	return h.e.Create(h.t.Context(), r)
}
func (h *klarnaRig) session() tender.Result {
	h.t.Helper()
	res, err := h.create()
	if err != nil || res.Payment.Status != tender.PaymentRequiresAction || res.Payment.Action.Mode != tender.ActionComponent || res.Operation.ProviderReference == "" || !strings.Contains(res.Payment.Action.Data, "client_token") {
		h.t.Fatalf("session = %+v %v", res, err)
	}
	return res
}
func (h *klarnaRig) callback(session string) string {
	h.t.Helper()
	for range 2 {
		resp, err := http.Post(h.url+"/sim/mock/authorize/"+session, "application/json", nil)
		if err != nil {
			h.t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 204 {
			h.t.Fatalf("synthetic callback = %d", resp.StatusCode)
		}
	}
	ids, err := h.a.Authorizations(h.t.Context(), "pay")
	if err != nil || len(ids) != 1 {
		h.t.Fatalf("durable receipts = %v %v", ids, err)
	}
	return ids[0]
}
func (h *klarnaRig) order() (tender.Result, error) {
	h.t.Helper()
	r := h.session()
	id := h.callback(r.Operation.ProviderReference)
	h.restart()
	return h.a.ProcessAuthorization(h.t.Context(), h.e, "pay", id)
}
func (h *klarnaRig) compare(res tender.Result, route string) {
	h.t.Helper()
	err := contract.Verify(h.t.Context(), h.sim, h.db, dbScope, res.Operation.ID, mockserver.KlarnaKey("mid", res.Operation.ProviderKey), func(r contract.Records) error {
		var fx mockserver.KlarnaRecord
		if err := json.Unmarshal(r.Fixture.Data, &fx); err != nil {
			return err
		}
		if fx.Route != route || fx.OrderID != r.Payment.ProviderReference || fx.Reference != r.Operation.ProviderReference || fx.Amount != r.Operation.Amount.Minor || fx.Currency != r.Payment.Amount.Currency {
			return fmt.Errorf("provider/host mismatch %s: %+v", route, fx)
		}
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
}
func TestKlarnaContract(t *testing.T) {
	run := func(t *testing.T, db adyenStore) {
		h := newKlarna(t, db, mockserver.KlarnaFixture{})
		ctx := t.Context()
		ord, err := h.order()
		if err != nil || ord.Payment.Status != tender.PaymentAuthorized || ord.Payment.Funds.Authorized != 100 || ord.Payment.VerifiedMethod != tender.MethodKlarna {
			t.Fatalf("placed = %+v %v", ord, err)
		}
		for _, step := range []struct {
			key          string
			minor, total int64
		}{{"first", 40, 40}, {"second", 60, 100}} {
			req := kreq(step.key, step.minor)
			res, err := h.e.Capture(ctx, req)
			if err != nil || res.Payment.Funds.Captured != step.total {
				t.Fatalf("capture = %+v %v", res, err)
			}
			h.compare(res, "captures")
			again, err := h.e.Capture(ctx, req)
			if err != nil || again.Operation.ID != res.Operation.ID {
				t.Fatal("capture replay", err)
			}
		}
		refund, err := h.e.Refund(ctx, kreq("refund", 35))
		if err != nil || refund.Payment.Funds.Refunded != 35 || refund.Payment.Funds.Captured != 100 {
			t.Fatalf("refund = %+v %v", refund, err)
		}
		h.compare(refund, "refunds")
		out, err := db.Outbox(ctx, dbScope)
		if err != nil || len(out) != 1 {
			t.Fatalf("outbox = %v %v", out, err)
		}
	}
	t.Run("memory", func(t *testing.T) { run(t, store.NewMemory()) })
	t.Run("postgres", func(t *testing.T) { run(t, postgres.New(postgresFixture(t)())) })
}
func TestKlarnaPolicyAndOutcomes(t *testing.T) {
	t.Run("strict checkout", func(t *testing.T) {
		h := newKlarna(t, store.NewMemory(), mockserver.KlarnaFixture{})
		cfg := h.cfg
		cfg.Scope.Environment = tender.EnvironmentSandbox
		cfg.BaseURL = "https://api-na.playground.klarna.com"
		cfg.CallbackURL = "https://shop.example/callback"
		cfg.APIKey = "klarna_test_api_example"
		a, err := klarna.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if a.Capabilities().Create || len(a.Capabilities().ActionModes) != 0 {
			t.Fatal("strict browser advertised Klarna")
		}
		cfg.BaseURL = "https://api-na.klarna.com"
		if _, err = klarna.New(cfg); !errors.Is(err, tender.ErrValidation) {
			t.Fatal("wrong host accepted", err)
		}
	})
	t.Run("ineligible", func(t *testing.T) {
		h := newKlarna(t, store.NewMemory(), mockserver.KlarnaFixture{Category: "pay_now"})
		r, err := h.create()
		if !errors.Is(err, tender.ErrUnsupported) || r.Payment.Status == tender.PaymentDeclined {
			t.Fatalf("ineligible = %+v %v", r, err)
		}
	})
	t.Run("declined", func(t *testing.T) {
		h := newKlarna(t, store.NewMemory(), mockserver.KlarnaFixture{RejectOrder: true})
		r, err := h.order()
		if !errors.Is(err, tender.ErrDeclined) || r.Payment.Status != tender.PaymentDeclined || r.Payment.Funds.Authorized != 0 {
			t.Fatalf("decline = %+v %v", r, err)
		}
	})
	t.Run("pending", func(t *testing.T) {
		h := newKlarna(t, store.NewMemory(), mockserver.KlarnaFixture{FraudStatus: "PENDING", FraudOnRead: "ACCEPTED"})
		r, err := h.order()
		if err != nil || r.Payment.Status != tender.PaymentPending || r.Operation.Status != tender.OperationPending {
			t.Fatalf("pending = %+v %v", r, err)
		}
		h.restart()
		r, err = h.e.Recover(t.Context(), r.Operation.ID)
		if err != nil || r.Payment.Status != tender.PaymentAuthorized {
			t.Fatalf("cleared = %+v %v", r, err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		h := newKlarna(t, store.NewMemory(), mockserver.KlarnaFixture{})
		r := h.session()
		id := h.callback(r.Operation.ProviderReference)
		h.now = h.now.Add(49 * time.Hour)
		r, err := h.a.ProcessAuthorization(t.Context(), h.e, "pay", id)
		if err == nil || errors.Is(err, tender.ErrDeclined) || r.Payment.Status != tender.PaymentExpired {
			t.Fatalf("expired = %+v %v", r, err)
		}
	})
	for _, captured := range []int64{0, 40} {
		t.Run(fmt.Sprint("cancel-", captured), func(t *testing.T) {
			h := newKlarna(t, store.NewMemory(), mockserver.KlarnaFixture{})
			if _, err := h.order(); err != nil {
				t.Fatal(err)
			}
			if captured != 0 {
				if _, err := h.e.Capture(t.Context(), kreq("capture", captured)); err != nil {
					t.Fatal(err)
				}
			}
			r, err := h.e.Void(t.Context(), kreq("void", 100-captured))
			if err != nil || r.Payment.Status != tender.PaymentVoided || r.Payment.Funds.Captured != captured {
				t.Fatalf("cancel = %+v %v", r, err)
			}
			if captured > 0 {
				r, err = h.e.Refund(t.Context(), kreq("refund", 20))
				if err != nil || r.Payment.Status != tender.PaymentVoided || r.Payment.Funds.Refunded != 20 {
					t.Fatal("refund of released order", r, err)
				}
			}
		})
	}
}
func TestKlarnaAuthorizationBindingAndRecovery(t *testing.T) {
	h := newKlarna(t, store.NewMemory(), mockserver.KlarnaFixture{})
	ctx := t.Context()
	session := h.session()
	id := h.callback(session.Operation.ProviderReference)
	seen := <-h.seen
	forged, _ := json.Marshal(map[string]string{"session_id": session.Operation.ProviderReference, "authorization_token": "untrusted"})
	resp, err := http.Post(h.url+"/callback?create="+session.Operation.ID+"&proof="+strings.Repeat("x", 43), "application/json", bytes.NewReader(forged))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatal("forged callback accepted")
	}
	resp, err = http.Post(seen.target, "application/json", strings.NewReader(`{"session_id":"different","authorization_token":"untrusted"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("wrong session accepted: %d", resp.StatusCode)
	}
	err = h.db.Transact(ctx, func(tx tender.Tx) error {
		rows, err := tx.(tender.VerifiedEvidenceReader).MatchingVerifiedEvidence(dbScope, "klarna", "mid", map[string]string{"payment": "pay"})
		if err != nil {
			return err
		}
		if len(rows) != 1 || bytes.Contains(rows[0], []byte(seen.token)) || bytes.Contains(rows[0], []byte("proof=")) {
			return errors.New("authorization receipt was duplicated or persisted a raw secret")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	pay, err := h.e.Get(ctx, "pay")
	if err != nil || pay.Status != tender.PaymentRequiresAction || pay.Funds != (tender.Funds{}) {
		t.Fatal("callback changed funds", pay, err)
	}
	if _, err := h.a.ProcessAuthorization(ctx, h.e, "pay", "unknown"); err == nil {
		t.Fatal("unbound token accepted")
	}
	ord, err := h.a.ProcessAuthorization(ctx, h.e, "pay", id)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.a.ProcessAuthorization(ctx, h.e, "pay", id)
	if err != nil || ord.Operation.ID != again.Operation.ID {
		t.Fatal("duplicate callback placed another order", again, err)
	}
	if err := h.sim.SetScenarios("klarna.captures", mockserver.Scenario{Fault: mockserver.LoseResponse}); err != nil {
		t.Fatal(err)
	}
	cap, err := h.e.Capture(ctx, kreq("lost", 37))
	if !errors.Is(err, tender.ErrUnknownOutcome) || cap.Payment.Funds.Captured != 0 {
		t.Fatalf("lost = %+v %v", cap, err)
	}
	h.restart()
	cap, err = h.e.Recover(ctx, cap.Operation.ID)
	if err != nil || cap.Payment.Funds.Captured != 37 {
		t.Fatalf("recovery = %+v %v", cap, err)
	}
	h.compare(cap, "captures")
}

type klarnaResponseFilter func(*http.Response)

func (f klarnaResponseFilter) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := http.DefaultTransport.RoundTrip(req)
	if err == nil {
		f(res)
	}
	return res, err
}

func TestKlarnaConflictingOrderEvidence(t *testing.T) {
	for _, fault := range []string{"transaction-location", "order-amount", "unattributed-capture", "wrong-order", "terminal-status"} {
		t.Run(fault, func(t *testing.T) {
			h := newKlarna(t, store.NewMemory(), mockserver.KlarnaFixture{})
			ctx := t.Context()
			if _, err := h.order(); err != nil {
				t.Fatal(err)
			}
			if fault != "transaction-location" {
				if err := h.sim.SetScenarios("klarna.captures", mockserver.Scenario{Fault: mockserver.LoseResponse}); err != nil {
					t.Fatal(err)
				}
			}
			h.cfg.Transport = klarnaResponseFilter(func(res *http.Response) {
				if fault == "transaction-location" && res.StatusCode == 201 {
					res.Header.Set("Location", "https://wrong.example/captures/other")
					return
				}
				if res.Request.Method != "GET" {
					return
				}
				var body map[string]any
				if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				switch fault {
				case "order-amount":
					body["order_amount"] = 999
				case "wrong-order":
					body["merchant_reference1"] = "other"
				case "unattributed-capture":
					body["captures"].([]any)[0].(map[string]any)["reference"] = "other"
				case "terminal-status":
					body["status"] = "CANCELLED"
				}
				raw, _ := json.Marshal(body)
				res.Body = io.NopCloser(bytes.NewReader(raw))
			})
			h.restart()
			cap, err := h.e.Capture(ctx, kreq("capture", 22))
			if !errors.Is(err, tender.ErrUnknownOutcome) || cap.Payment.Funds.Captured != 0 {
				t.Fatalf("ambiguous capture accepted: %+v %v", cap, err)
			}
			if fault != "transaction-location" {
				r, err := h.e.Recover(ctx, cap.Operation.ID)
				if !errors.Is(err, tender.ErrUnknownOutcome) || r.Payment.Funds.Captured != 0 {
					t.Fatalf("conflicting order accepted: %+v %v", r, err)
				}
			}
			h.cfg.Transport = nil
			h.restart()
			r, err := h.e.Recover(ctx, cap.Operation.ID)
			if err != nil || r.Payment.Funds.Captured != 22 {
				t.Fatal("unmodified record did not recover", r, err)
			}
		})
	}
}

func TestKlarnaCallbackExpiryAndCartBinding(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint("changed-cart-", changed), func(t *testing.T) {
			h := newKlarna(t, store.NewMemory(), mockserver.KlarnaFixture{})
			r := h.session()
			id := h.callback(r.Operation.ProviderReference)
			if changed {
				h.cfg.OrderLines = func(context.Context, tender.Payment) ([]klarna.Line, error) {
					return []klarna.Line{{Type: "digital", Name: "Other course", Quantity: 1, UnitPrice: 100, TotalAmount: 100}}, nil
				}
			} else {
				h.now = h.now.Add(61 * time.Minute)
			}
			h.restart()
			r, err := h.a.ProcessAuthorization(t.Context(), h.e, "pay", id)
			if !errors.Is(err, tender.ErrValidation) || r.Payment.Funds != (tender.Funds{}) || r.Payment.ProviderReference != "" {
				t.Fatal("ineligible receipt changed order", r, err)
			}
		})
	}
}

func TestKlarnaUSTaxAndInactiveMerchant(t *testing.T) {
	for _, inactive := range []bool{false, true} {
		t.Run(fmt.Sprint("inactive-", inactive), func(t *testing.T) {
			h := newKlarna(t, store.NewMemory(), mockserver.KlarnaFixture{})
			h.cfg.OrderLines = func(context.Context, tender.Payment) ([]klarna.Line, error) {
				return []klarna.Line{{Type: "physical", Name: "Item", Quantity: 1, UnitPrice: 91, TotalAmount: 91}, {Type: "sales_tax", Name: "Sales Tax", Quantity: 1, UnitPrice: 9, TotalAmount: 9}}, nil
			}
			h.cfg.Transport = klarnaRequestFunc(func(req *http.Request) (*http.Response, error) {
				raw, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				req.Body = io.NopCloser(bytes.NewReader(raw))
				var purchase struct {
					Amount int64  `json:"order_amount"`
					Tax    *int64 `json:"order_tax_amount"`
				}
				if json.Unmarshal(raw, &purchase) != nil || purchase.Amount != 100 || purchase.Tax == nil || *purchase.Tax != 9 {
					t.Error("US order and sales tax must match the host's 100/9 minor units")
				}
				if inactive {
					return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error_code":"MERCHANT_INACTIVE"}`)), Request: req}, nil
				}
				return http.DefaultTransport.RoundTrip(req)
			})
			h.restart()
			r, err := h.create()
			if inactive {
				if !errors.Is(err, tender.ErrUnsupported) || r.Payment.Status == tender.PaymentDeclined {
					t.Fatalf("merchant configuration = %v %v", r.Payment.Status, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

type klarnaRequestFunc func(*http.Request) (*http.Response, error)

func (f klarnaRequestFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
