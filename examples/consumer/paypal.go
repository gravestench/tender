package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/providers/paypal"
)

//go:embed paypal.html
var walletHTML string

//go:embed paypal.js
var walletJS string
var walletPage = template.Must(template.New("paypal").Parse(walletHTML))

func init() { mockCheckoutInstallers["paypal"] = (*app).installPayPal }

type fixtureWalletTransport struct {
	key, base string
	transport *http.Transport
}

func (f fixtureWalletTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme+"://"+r.URL.Host != f.base {
		return nil, errors.New("untrusted fixture destination")
	}
	req := r.Clone(r.Context())
	req.Header.Set("X-Fixture-Access", f.key)
	return f.transport.RoundTrip(req)
}

type walletCheckout struct {
	*app
	client      *http.Client
	component   bool
	operatorKey string
	sim         *mockserver.Server
}

func (a *app) installPayPal(mux *http.ServeMux, storage tender.Store) (func(), error) {
	if a.scope.Environment != tender.EnvironmentMock || a.ledger == nil {
		return nil, errors.New("consumer: mock PayPal requires a migrated local SQL database")
	}
	mode := os.Getenv("TENDER_PAYPAL_MOCK_PRESENTATION")
	if mode != "" && mode != "component" && mode != "popup" && mode != "redirect" {
		return nil, errors.New("consumer: unsupported fixture presentation")
	}
	operatorKey := os.Getenv("TENDER_PAYPAL_OPERATOR_KEY")
	if operatorKey != "" && len(operatorKey) < 32 {
		return nil, errors.New("consumer: PayPal operator key must have at least 32 characters")
	}
	a.ledger.SetMaxOpenConns(1)
	a.provider, a.account = "paypal", "TENDERMOCKSELLER"
	a.market.Method = tender.MethodPayPal
	transport := fixtureWalletTransport{rand.Text(), a.origin, &http.Transport{IdleConnTimeout: 30 * time.Second}}
	c := paypal.Config{Scope: a.scope, MerchantAccount: a.account, BaseURL: a.origin + "/simulator", ClientID: "local-wallet", Secret: rand.Text(), WebhookID: "WH-MOCK", Transport: transport}
	wallet := &walletCheckout{app: a, client: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, component: mode == "" || mode == "component", operatorKey: operatorKey}
	adapter, err := paypal.New(c)
	if err != nil {
		return nil, err
	}
	a.engine, err = tender.NewEngine(tender.EngineConfig{Provider: adapter, Store: storage, Scope: a.scope, Market: a.market, ProviderName: a.provider, MerchantAccount: a.account})
	if err != nil {
		return nil, err
	}
	wake := make(chan struct{}, 1)
	hook, err := adapter.Webhook(storage, a.engine, func(string) {
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	if err != nil {
		return nil, err
	}
	target := a.origin + "/webhooks/paypal"
	sim, err := mockserver.New(mockserver.Config{PublicBaseURL: c.BaseURL, AllowedCallbackTargets: []string{target}, SigningKey: []byte(rand.Text()), Routes: mockserver.PayPalRoutes(mockserver.PayPalFixture{MerchantAccount: c.MerchantAccount, ClientID: c.ClientID, Secret: c.Secret, WebhookID: c.WebhookID, WebhookURL: target})})
	if err != nil {
		return nil, err
	}
	lost := os.Getenv("TENDER_PAYPAL_MOCK_LOST_RESPONSE")
	if lost != "" && lost != "capture" {
		sim.Close()
		return nil, errors.New("consumer: invalid PayPal lost response route")
	}
	if lost == "capture" {
		if err := sim.SetScenarios("paypal.capture", mockserver.Scenario{Fault: mockserver.LoseResponse}); err != nil {
			sim.Close()
			return nil, err
		}
	}
	wallet.sim = sim
	vendor := http.StripPrefix("/simulator", sim.Handler())
	mux.HandleFunc("/simulator/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fixture-Access") != transport.key {
			http.NotFound(w, r)
			return
		}
		vendor.ServeHTTP(w, r)
	})
	mux.Handle("/webhooks/paypal", hook)
	mux.HandleFunc("GET /orders/{order}", a.owned(wallet.page))
	mux.HandleFunc("POST /orders/{order}/wallet/{step}", a.owned(wallet.submit))
	mux.HandleFunc("POST /operator/wallet/orders/{order}/refund", wallet.refund)
	mux.HandleFunc("GET /operator/wallet/orders/{order}/fixture", wallet.fixture)
	mux.HandleFunc("GET /paypal.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		io.WriteString(w, walletJS)
	})
	stop := a.startCheckoutWorker(wake, hook)
	return func() { stop(); sim.Close(); transport.transport.CloseIdleConnections() }, nil
}

// The local merchant diagnostic shows only this process's synthetic provider
// record. It is never mounted for ordinary PayPal environments.
func (wc *walletCheckout) fixture(w http.ResponseWriter, r *http.Request) {
	if wc.operatorKey == "" || r.Header.Get("Origin") != "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+wc.operatorKey)) != 1 {
		http.NotFound(w, r)
		return
	}
	for _, o := range orders {
		if r.PathValue("order") != o.ID {
			continue
		}
		p, err := wc.engine.Get(r.Context(), o.PaymentID)
		if err != nil || p.OrderID != o.ID || p.ProviderReference == "" {
			http.NotFound(w, r)
			return
		}
		rec, ok := wc.sim.Lookup("paypal/order/" + p.ProviderReference)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(rec.Data)
		return
	}
	http.NotFound(w, r)
}

// This route models a host decision after fulfillment. It is outside the
// shopper session and uses one fixed partial amount and one replay-safe key.
func (wc *walletCheckout) refund(w http.ResponseWriter, r *http.Request) {
	if wc.operatorKey == "" || r.Header.Get("Origin") != "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+wc.operatorKey)) != 1 {
		http.NotFound(w, r)
		return
	}
	for _, o := range orders {
		if r.PathValue("order") != o.ID {
			continue
		}
		_, err := wc.engine.Refund(r.Context(), tender.Request{PaymentID: o.PaymentID, Amount: tender.Money{Minor: 1000, Currency: o.Total.Currency}, Key: "wallet/refund/partial/" + o.ID})
		if err != nil {
			http.Error(w, "inspect the stored refund before retrying", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	http.NotFound(w, r)
}
func (wc *walletCheckout) action(p tender.Payment, o order) bool {
	if !wc.component || p.Scope != wc.scope || p.MerchantAccount != wc.account || p.Status != tender.PaymentRequiresAction || p.Provider != "paypal" || p.OrderID != o.ID || p.Amount != o.Total || p.Action.Mode != tender.ActionComponent || p.Action.Data != p.ProviderReference || p.Action.Validate(time.Now()) != nil {
		return false
	}
	// MOCK records live in this process. After restart an abandoned action
	// cannot be completed against the old fixture order.
	rec, ok := wc.sim.Lookup("paypal/order/" + p.ProviderReference)
	if !ok {
		return false
	}
	var fixture struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(rec.Data, &fixture) == nil && (fixture.Status == "PAYER_ACTION_REQUIRED" || fixture.Status == "APPROVED")
}
func (wc *walletCheckout) state(ctx context.Context, o order) (string, error) {
	if !wc.component {
		return "unavailable", nil
	}
	p, err := wc.engine.Get(ctx, o.PaymentID)
	if errors.Is(err, tender.ErrNotFound) {
		return "unpaid", nil
	}
	if err != nil {
		return "", err
	}
	if wc.action(p, o) {
		return "requires_action", nil
	}
	if p.Status == tender.PaymentRequiresAction {
		return "unavailable", nil
	}
	return string(p.Status), nil
}
func (wc *walletCheckout) page(w http.ResponseWriter, r *http.Request, o order, s session) {
	status, err := wc.state(r.Context(), o)
	if err != nil {
		http.Error(w, "Wallet unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-CSRF-Token", s.csrf)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	_ = walletPage.Execute(w, struct{ ID, CSRF, Status, Price string }{o.ID, s.csrf, status, fmt.Sprintf("$%d.%02d", o.Total.Minor/100, o.Total.Minor%100)})
}
func (wc *walletCheckout) submit(w http.ResponseWriter, r *http.Request, o order, s session) {
	if _, ok := wc.form(w, r, s, false); !ok {
		return
	}
	if !wc.component {
		http.Error(w, "Wallet unavailable", 409)
		return
	}
	ctx := r.Context()
	var err error
	status := ""
	switch step := r.PathValue("step"); step {
	case "start":
		_, err = wc.engine.Create(ctx, tender.Request{OrderID: o.ID, PaymentID: o.PaymentID, Amount: o.Total, Key: "wallet/create/" + o.ID})
	case "approve", "cancel":
		p, getErr := wc.engine.Get(ctx, o.PaymentID)
		if getErr != nil || !wc.action(p, o) {
			http.Error(w, "Wallet action unavailable", 409)
			return
		}
		buyerReq, reqErr := http.NewRequestWithContext(ctx, "POST", wc.origin+"/simulator/fixture/orders/"+p.ProviderReference+"/"+step, nil)
		if reqErr != nil {
			http.Error(w, "Wallet unavailable", 503)
			return
		}
		buyerReq.Header.Set("Content-Type", "application/json")
		reply, sendErr := wc.client.Do(buyerReq)
		if sendErr != nil {
			http.Error(w, "Wallet unavailable", 503)
			return
		}
		reply.Body.Close()
		if reply.StatusCode != 204 {
			http.Error(w, "Wallet action unavailable", 409)
			return
		}
		if step == "cancel" {
			status = "cancelled"
		} else {
			status = "approved"
		}
	case "confirm":
		p, getErr := wc.engine.Get(ctx, o.PaymentID)
		if getErr != nil || !wc.action(p, o) {
			http.Error(w, "Wallet action unavailable", 409)
			return
		}
		// Buyer approval can outlive the browser request. Only this independently
		// authenticated order confirmation can collect funds.
		_, err = wc.engine.Confirm(ctx, tender.Request{PaymentID: o.PaymentID, Amount: o.Total, Key: "wallet/confirm/" + rand.Text()})
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Wallet operation unavailable", 409)
		return
	}
	if status == "" {
		status, err = wc.state(ctx, o)
	}
	if err != nil || wc.project(ctx, o) != nil {
		http.Error(w, "Wallet state unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}
