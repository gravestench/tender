package main

import (
	"context"
	"crypto/rand"
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
	client    *http.Client
	component bool
}

func (a *app) installPayPal(mux *http.ServeMux, storage tender.Store) (func(), error) {
	if a.scope.Environment != tender.EnvironmentMock || a.ledger == nil {
		return nil, errors.New("consumer: mock PayPal requires a migrated local SQL database")
	}
	mode := os.Getenv("TENDER_PAYPAL_MOCK_PRESENTATION")
	if mode != "" && mode != "component" && mode != "popup" && mode != "redirect" {
		return nil, errors.New("consumer: unsupported fixture presentation")
	}
	a.ledger.SetMaxOpenConns(1)
	a.provider, a.account = "paypal", "TENDERMOCKSELLER"
	a.market.Method = tender.MethodPayPal
	transport := fixtureWalletTransport{rand.Text(), a.origin, &http.Transport{IdleConnTimeout: 30 * time.Second}}
	c := paypal.Config{Scope: a.scope, MerchantAccount: a.account, BaseURL: a.origin + "/simulator", ClientID: "local-wallet", Secret: rand.Text(), WebhookID: "WH-MOCK", Transport: transport}
	wallet := &walletCheckout{app: a, client: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, component: mode == "" || mode == "component"}
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
	mux.HandleFunc("GET /paypal.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		io.WriteString(w, walletJS)
	})
	stop := a.startCheckoutWorker(wake, hook)
	return func() { stop(); sim.Close(); transport.transport.CloseIdleConnections() }, nil
}
func (wc *walletCheckout) action(p tender.Payment, o order) bool {
	return wc.component && p.Scope == wc.scope && p.MerchantAccount == wc.account && p.Status == tender.PaymentRequiresAction && p.Provider == "paypal" && p.OrderID == o.ID && p.Amount == o.Total && p.Action.Mode == tender.ActionComponent && p.Action.Data == p.ProviderReference && p.Action.Validate(time.Now()) == nil
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
			_, err = wc.engine.Confirm(ctx, tender.Request{PaymentID: o.PaymentID, Amount: o.Total, Key: "wallet/confirm/" + o.ID})
		}
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
