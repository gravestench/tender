package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/providers/klarna"
)

func init() { mockCheckoutInstallers["klarna"] = (*app).installKlarna }

var klarnaPage = template.Must(template.New("klarna").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><title>Synthetic Klarna checkout</title>
<main><h1>Synthetic Klarna checkout</h1><p>{{.Price}}</p><p role="status">{{.Status}}</p>
{{if .Button}}<button type="button" data-path="{{.Path}}" data-csrf="{{.CSRF}}">{{.Button}}</button>{{end}}</main>
<script src="/klarna-checkout.js"></script></html>`))

const klarnaJS = `
document.addEventListener("click", async event => {
 const button = event.target.closest("button[data-path]");
 if (!button || button.disabled) return;
 button.disabled = true;
 try {
   const response = await fetch(location.pathname + "/" + button.dataset.path, {
     method: "POST", headers: {"Content-Type": "application/x-www-form-urlencoded"},
     body: new URLSearchParams({csrf:button.dataset.csrf}), redirect:"error"
   });
   if (!response.ok) throw Error("unavailable");
   const next = new DOMParser().parseFromString(await response.text(), "text/html");
   document.querySelector("main").replaceWith(next.querySelector("main"));
 } catch {
   document.querySelector("[role=status]").textContent="Financing unavailable. Refresh to check this order.";
 }
});`

func (a *app) installKlarna(mux *http.ServeMux, db tender.Store) (func(), error) {
	if a.scope.Environment != tender.EnvironmentMock || a.ledger == nil {
		return nil, errors.New("consumer: synthetic Klarna requires MOCK and a migrated SQL database")
	}
	a.ledger.SetMaxOpenConns(1)
	a.provider, a.account, a.market.Method = "klarna", "FixtureMID", tender.MethodKlarna
	category := os.Getenv("TENDER_KLARNA_MOCK_CATEGORY")
	if category != "" && category != "pay_now" && category != klarna.Category {
		return nil, errors.New("consumer: unsupported synthetic Klarna category")
	}
	fraud := os.Getenv("TENDER_KLARNA_MOCK_FRAUD_STATUS")
	if fraud != "" && fraud != "ACCEPTED" && fraud != "PENDING" && fraud != "REJECTED" {
		return nil, errors.New("consumer: unsupported synthetic Klarna fraud status")
	}
	var clockOffset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(clockOffset.Load())) }
	cb, base := a.origin+"/callbacks/klarna", a.origin+"/simulator"
	fx := mockserver.KlarnaFixture{MerchantAccount: a.account, APIKey: "mock-only-klarna", CallbackURL: cb, Category: category, FraudStatus: fraud}
	secret := sha256.Sum256([]byte("Tender local synthetic Klarna callback fixture"))
	adapter, err := klarna.New(klarna.Config{Scope: a.scope, Store: db, MerchantAccount: a.account, APIKey: fx.APIKey, BaseURL: base, CallbackURL: cb, CallbackKey: secret[:], Now: now, OrderLines: func(_ context.Context, p tender.Payment) ([]klarna.Line, error) {
		for _, o := range orders {
			if o.PaymentID == p.ID && o.ID == p.OrderID && o.Total == p.Amount {
				return []klarna.Line{{Type: "digital", Name: "Synthetic course", Reference: o.ID, Quantity: 1, UnitPrice: o.Total.Minor, TotalAmount: o.Total.Minor}}, nil
			}
		}
		return nil, tender.ErrValidation
	}})
	if err != nil {
		return nil, err
	}
	a.engine, err = tender.NewEngine(tender.EngineConfig{Store: db, Provider: adapter, Scope: a.scope, Market: a.market, MerchantAccount: a.account, ProviderName: a.provider, Now: now})
	if err != nil {
		return nil, err
	}
	sim, err := mockserver.New(mockserver.Config{PublicBaseURL: base, Clock: now, Routes: mockserver.KlarnaRoutes(fx)})
	if err != nil {
		return nil, err
	}
	mux.Handle("/simulator/", http.StripPrefix("/simulator", sim.Handler()))
	// An explicitly enabled local fixture operator models the merchant's
	// capture/refund decisions. Buyer forms never choose amounts or mutations.
	if operatorKey := os.Getenv("TENDER_KLARNA_MOCK_OPERATOR_TOKEN"); len(operatorKey) >= 32 {
		mux.HandleFunc("POST /fixture/klarna/orders/{order}/{action}", func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Tender-Mock-Operator")), []byte(operatorKey)) != 1 {
				http.Error(w, "operator denied", http.StatusForbidden)
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
			name := r.PathValue("action")
			switch name {
			case "advance-expiry":
				clockOffset.Store(int64(49 * time.Hour))
				w.WriteHeader(http.StatusNoContent)
				return
			case "lose-order", "lose-capture":
				route := "klarna.order"
				if name == "lose-capture" {
					route = "klarna.captures"
				}
				if err := sim.SetScenarios(route, mockserver.Scenario{Fault: mockserver.LoseResponse}); err != nil {
					http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			case "recover-capture-all":
				res, err := a.engine.Capture(r.Context(), tender.Request{PaymentID: o.PaymentID, Key: "klarna-fixture/capture-all/" + o.ID, Amount: o.Total})
				if errors.Is(err, tender.ErrUnknownOutcome) && res.Operation.ID != "" {
					_, err = a.engine.Recover(r.Context(), res.Operation.ID)
				}
				if err != nil || a.project(r.Context(), o) != nil {
					http.Error(w, "capture recovery unavailable", http.StatusConflict)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			amount := o.Total.Minor
			var mutate func(context.Context, tender.Request) (tender.Result, error)
			switch name {
			case "capture-half":
				amount, mutate = amount/2, a.engine.Capture
			case "capture-rest":
				amount, mutate = amount-amount/2, a.engine.Capture
			case "capture-all":
				mutate = a.engine.Capture
			case "refund-half":
				amount, mutate = amount/2, a.engine.Refund
			case "cancel":
				mutate = a.engine.Void
			case "release":
				amount, mutate = amount-amount/2, a.engine.Void
			default:
				http.NotFound(w, r)
				return
			}
			result, err := mutate(r.Context(), tender.Request{PaymentID: o.PaymentID, Key: "klarna-fixture/" + name + "/" + o.ID, Amount: tender.Money{Currency: o.Total.Currency, Minor: amount}})
			if err != nil {
				http.Error(w, "operation unavailable", http.StatusConflict)
				return
			}
			if err := a.project(r.Context(), o); err != nil {
				http.Error(w, "fulfillment pending", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("X-Tender-Mock-Provider-Order", result.Payment.ProviderReference)
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.Handle("/callbacks/klarna", adapter.CallbackHandler())
	mux.HandleFunc("GET /klarna-checkout.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		fmt.Fprint(w, klarnaJS)
	})
	render := func(w http.ResponseWriter, r *http.Request, o order, s session) {
		p, err := a.engine.Get(r.Context(), o.PaymentID)
		view := struct{ Price, Status, Button, Path, CSRF string }{Price: fmt.Sprintf("$%d.%02d", o.Total.Minor/100, o.Total.Minor%100), CSRF: s.csrf}
		switch {
		case errors.Is(err, tender.ErrNotFound):
			view.Status, view.Button, view.Path = "Unpaid", "Start synthetic financing", "prepare"
		case err != nil || p.OrderID != o.ID || p.Amount != o.Total:
			http.Error(w, "checkout unavailable", 503)
			return
		default:
			view.Status = string(p.Status)
			if _, err := adapter.CheckoutAction(p); err == nil {
				view.Button, view.Path = "Approve synthetic purchase", "approve"
			} else if p.Status == tender.PaymentRequiresAction || p.Status == tender.PaymentCreated {
				view.Status = "Financing unavailable"
			}
		}
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-CSRF-Token", s.csrf)
		_ = klarnaPage.Execute(w, view)
	}
	mux.HandleFunc("GET /orders/{order}", a.owned(render))
	mux.HandleFunc("POST /orders/{order}/prepare", a.owned(func(w http.ResponseWriter, r *http.Request, o order, s session) {
		if _, ok := a.form(w, r, s, false); !ok {
			return
		}
		_, err := a.engine.Create(r.Context(), tender.Request{PaymentID: o.PaymentID, OrderID: o.ID, Key: "klarna-session/" + o.ID, Amount: o.Total})
		if err != nil && !errors.Is(err, tender.ErrUnsupported) {
			http.Error(w, "session unavailable", 503)
			return
		}
		render(w, r, o, s)
	}))
	worker := &klarnaWorker{app: a, adapter: adapter}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	mux.HandleFunc("POST /orders/{order}/approve", a.owned(func(w http.ResponseWriter, r *http.Request, o order, s session) {
		if _, ok := a.form(w, r, s, false); !ok {
			return
		}
		p, err := a.engine.Get(r.Context(), o.PaymentID)
		if err != nil || p.OrderID != o.ID || p.Amount != o.Total {
			http.Error(w, "order unavailable", 409)
			return
		}
		action, err := adapter.CheckoutAction(p)
		if err != nil {
			http.Error(w, "approval unavailable", 409)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), "POST", base+"/mock/authorize/"+action.SessionID, nil)
		if err != nil {
			http.Error(w, "approval unavailable", 503)
			return
		}
		res, err := client.Do(req)
		if err != nil {
			http.Error(w, "approval unavailable", 503)
			return
		}
		res.Body.Close()
		if res.StatusCode != 204 {
			http.Error(w, "order pending", 503)
			return
		}
		// A session may have older expired/superseded authorization receipts.
		// Drain the same durable inbox as the background worker.
		a.processCheckoutEvents(r.Context(), worker)
		render(w, r, o, s)
	}))
	// The worker drains durable receipts even if the browser disconnects.
	stop := a.startCheckoutWorker(nil, worker)
	return func() { stop(); client.CloseIdleConnections(); sim.Close() }, nil
}

type klarnaWorker struct {
	app     *app
	adapter *klarna.Adapter
	mu      sync.Mutex
}

func (w *klarnaWorker) Process(ctx context.Context, id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var payment string
	a := w.app
	err := a.ledger.QueryRowContext(ctx, `SELECT evidence->>'payment' FROM tender_provider_events WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND event_id=$5`, a.scope.MerchantID, a.scope.Environment, a.provider, a.account, id).Scan(&payment)
	if err != nil {
		return err
	}
	_, err = w.adapter.ProcessAuthorization(ctx, a.engine, payment, id)
	return err
}
