package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/providers/ach"
	"github.com/gravestench/tender/providers/adyen"
	"github.com/gravestench/tender/webhook"
)

func init() { mockCheckoutInstallers["adyen_ach"] = (*app).installACH }

var achPage = template.Must(template.New("ach").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><title>Synthetic ACH checkout</title><main><h1>Synthetic ACH checkout</h1><p role="status">{{.Status}}</p>{{if .Start}}<form method="post" action="{{.Path}}/ach/start"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>Start synthetic bank debit</button></form>{{end}}{{if .Consent}}<p>This synthetic consent authorizes one USD debit. No bank details are collected.</p><form method="post" action="{{.Path}}/ach/consent/approve"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>Approve synthetic bank debit</button></form><form method="post" action="{{.Path}}/ach/consent/deny"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>Decline synthetic bank debit</button></form>{{end}}</main></html>`))

type achCheckout struct {
	*app
	sim         *mockserver.Server
	adapter     *adyen.ACH
	hook        *webhook.Adyen
	store       tender.Store
	client      *http.Client
	operatorKey string
	mu          sync.Mutex
	sessions    map[string]string
}

type achFixtureTransport struct {
	origin, key string
	base        http.RoundTripper
}

func (t achFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme+"://"+r.URL.Host != t.origin {
		return nil, errors.New("ACH fixture destination denied")
	}
	req := r.Clone(r.Context())
	req.Header.Set("X-Fixture-Access", t.key)
	return t.base.RoundTrip(req)
}

func (a *app) installACH(mux *http.ServeMux, storage tender.Store) (func(), error) {
	if a.scope.Environment != tender.EnvironmentMock || a.ledger == nil {
		return nil, errors.New("consumer: ACH requires a migrated MOCK SQL store")
	}
	mode := os.Getenv("TENDER_ACH_MOCK_RESULT")
	if mode != "" && mode != "validation-refusal" && mode != "pending" {
		return nil, errors.New("consumer: invalid ACH fixture result")
	}
	operatorKey := os.Getenv("TENDER_CONSUMER_OPERATOR_KEY")
	if operatorKey != "" && len(operatorKey) < 32 {
		return nil, errors.New("consumer: operator key must have at least 32 characters")
	}
	a.ledger.SetMaxOpenConns(1)
	a.provider, a.account = "adyen", "TenderFixture"
	a.market.Method = tender.MethodACH
	access := rand.Text()
	baseTransport := &http.Transport{}
	transport := achFixtureTransport{origin: a.origin, key: access, base: baseTransport}
	cfg := adyen.Config{Environment: tender.EnvironmentMock, Origin: a.origin, ReturnURL: a.origin + "/checkout-return", BaseURL: a.origin + "/simulator/v72", APIKey: "synthetic-only", MerchantAccount: a.account, Transport: transport}
	adapter, err := adyen.NewACH(cfg)
	if err != nil {
		return nil, err
	}
	a.engine, err = tender.NewEngine(tender.EngineConfig{Provider: adapter, Store: storage, Scope: a.scope, Market: a.market, ProviderName: a.provider, MerchantAccount: a.account})
	if err != nil {
		return nil, err
	}
	secret := sha256.Sum256([]byte("Tender offline ACH fixture only"))
	key := hex.EncodeToString(secret[:])
	wake := make(chan struct{}, 1)
	hook, err := webhook.NewAdyen(webhook.AdyenConfig{Store: storage, Engine: a.engine, Scope: a.scope, MerchantAccount: a.account, HMACKeys: []string{key}, OnStored: func(string) {
		select {
		case wake <- struct{}{}:
		default:
		}
	}})
	if err != nil {
		return nil, err
	}
	target := a.origin + "/webhooks/adyen"
	sim, err := mockserver.New(mockserver.Config{PublicBaseURL: a.origin + "/simulator", SigningKey: secret[:], AllowedCallbackTargets: []string{target}, Routes: mockserver.ACHRoutes(mockserver.ACHFixture{MerchantAccount: a.account, APIKey: cfg.APIKey, WebhookURL: target, ValidationRefused: mode == "validation-refusal", PendingResult: mode == "pending"})})
	if err != nil {
		return nil, err
	}
	for _, route := range []string{"ach.consent", "ach.return"} {
		if err := sim.SetScenarios(route, mockserver.Scenario{DuplicateCallbacks: true}); err != nil {
			sim.Close()
			return nil, err
		}
	}
	ac := &achCheckout{app: a, sim: sim, adapter: adapter, hook: hook, store: storage, operatorKey: operatorKey, sessions: map[string]string{}, client: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	vendor := http.StripPrefix("/simulator", sim.Handler())
	mux.HandleFunc("/simulator/", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Fixture-Access")), []byte(access)) != 1 {
			http.NotFound(w, r)
			return
		}
		vendor.ServeHTTP(w, r)
	})
	mux.Handle("/webhooks/adyen", hook)
	mux.HandleFunc("GET /orders/{order}", a.owned(ac.page))
	mux.HandleFunc("POST /orders/{order}/ach/start", a.owned(ac.start))
	mux.HandleFunc("POST /orders/{order}/ach/consent/{decision}", a.owned(ac.consent))
	mux.HandleFunc("POST /operator/ach/orders/{order}/{operation}", ac.operate)
	stop := a.startCheckoutWorker(wake, hook)
	return func() { stop(); sim.Close(); baseTransport.CloseIdleConnections() }, nil
}

func (ac *achCheckout) page(w http.ResponseWriter, r *http.Request, o order, s session) {
	p, err := ac.engine.Get(r.Context(), o.PaymentID)
	view := struct {
		Status, Path, CSRF string
		Start, Consent     bool
	}{Path: "/orders/" + o.ID, CSRF: s.csrf}
	switch {
	case errors.Is(err, tender.ErrNotFound):
		view.Status, view.Start = "Unpaid", true
	case err != nil || p.OrderID != o.ID || p.Amount != o.Total || p.Market != ac.market:
		http.Error(w, "checkout unavailable", 503)
		return
	default:
		view.Status = string(p.Status)
		_, actionErr := ac.adapter.CheckoutSession(p)
		view.Consent = actionErr == nil
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	_ = achPage.Execute(w, view)
}

func (ac *achCheckout) start(w http.ResponseWriter, r *http.Request, o order, s session) {
	if _, ok := ac.form(w, r, s, false); !ok {
		return
	}
	_, err := ac.engine.Create(r.Context(), tender.Request{PaymentID: o.PaymentID, OrderID: o.ID, Key: "ach/start/" + o.ID, Amount: o.Total})
	if err != nil {
		http.Error(w, "bank debit unavailable", 409)
		return
	}
	ac.page(w, r, o, s)
}

func (ac *achCheckout) consent(w http.ResponseWriter, r *http.Request, o order, s session) {
	if _, ok := ac.form(w, r, s, false); !ok {
		return
	}
	decision := r.PathValue("decision")
	if decision != "approve" && decision != "deny" {
		http.NotFound(w, r)
		return
	}
	p, err := ac.engine.Get(r.Context(), o.PaymentID)
	if err != nil || p.OrderID != o.ID || p.Amount != o.Total {
		http.Error(w, "bank consent unavailable", 409)
		return
	}
	action, err := ac.adapter.CheckoutSession(p)
	if err != nil {
		http.Error(w, "bank consent unavailable", 409)
		return
	}
	res, err := ac.fixture(r.Context(), http.MethodPost, "/mock/consent/"+action.ID+"/"+decision)
	if err != nil || res.StatusCode != http.StatusOK {
		if res != nil {
			res.Body.Close()
		}
		http.Error(w, "fixture consent unavailable", 503)
		return
	}
	var receipt struct{ SessionResult string }
	err = json.NewDecoder(io.LimitReader(res.Body, 256)).Decode(&receipt)
	res.Body.Close()
	if err != nil || receipt.SessionResult == "" {
		http.Error(w, "fixture consent unavailable", 503)
		return
	}
	ac.mu.Lock()
	ac.sessions[o.ID] = action.ID
	ac.mu.Unlock()
	_, err = ac.engine.Confirm(r.Context(), tender.Request{PaymentID: o.PaymentID, Key: "ach/result/" + o.ID, Amount: o.Total, Token: receipt.SessionResult})
	if err != nil && !errors.Is(err, tender.ErrDeclined) {
		http.Error(w, "bank result unavailable", 409)
		return
	}
	ac.processCheckoutEvents(r.Context(), ac.hook)
	ac.page(w, r, o, s)
}

func (ac *achCheckout) fixture(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, ac.origin+"/simulator"+path, nil)
	if err != nil {
		return nil, err
	}
	return ac.client.Do(req)
}

func (ac *achCheckout) operate(w http.ResponseWriter, r *http.Request) {
	if ac.operatorKey == "" || r.Header.Get("Origin") != "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+ac.operatorKey)) != 1 {
		http.NotFound(w, r)
		return
	}
	var o order
	for _, candidate := range orders {
		if candidate.ID == r.PathValue("order") {
			o = candidate
			break
		}
	}
	if o.ID == "" {
		http.NotFound(w, r)
		return
	}
	op := r.PathValue("operation")
	if op == "void" || op == "refund" {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if op != "settle" && op != "return" {
		http.NotFound(w, r)
		return
	}
	ac.mu.Lock()
	id := ac.sessions[o.ID]
	ac.mu.Unlock()
	if id == "" {
		http.Error(w, "fixture session unavailable", 409)
		return
	}
	if op == "return" {
		res, err := ac.fixture(r.Context(), http.MethodPost, "/mock/return/"+id)
		if err != nil || res.StatusCode != http.StatusAccepted {
			if res != nil {
				res.Body.Close()
			}
			http.Error(w, "fixture return unavailable", 503)
			return
		}
		res.Body.Close()
	}
	// Provider callbacks are queued independently; a delayed callback may
	// arrive after this attempt. Retrying this host-only operation is safe.
	deadline := time.Now().Add(3 * time.Second)
	for {
		ac.processCheckoutEvents(r.Context(), ac.hook)
		kind := "Settled"
		if op == "return" {
			kind = "Chargeback"
		}
		res, err := ac.fixture(r.Context(), http.MethodGet, "/mock/report/"+id+"/"+kind)
		if err == nil && res.StatusCode == http.StatusOK {
			line, mapErr := ach.ReadSettlement(res.Body)
			res.Body.Close()
			if mapErr == nil {
				err = ach.ApplySettlement(r.Context(), ac.store, ac.engine, ac.scope, ac.account, "settlement_detail_report_batch_7.csv", line)
				if err == nil {
					_ = ac.project(r.Context(), o)
					w.WriteHeader(http.StatusAccepted)
					return
				}
			}
		} else if res != nil {
			res.Body.Close()
		}
		if time.Now().After(deadline) {
			http.Error(w, "report projection pending", 409)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
}
