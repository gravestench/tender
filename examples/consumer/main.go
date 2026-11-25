// Command consumer serves host-owned synthetic and Adyen test checkouts.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/store"
	"github.com/gravestench/tender/store/postgres"
)

//go:embed checkout.html
var checkoutHTML string

var checkout = template.Must(template.New("checkout").Parse(checkoutHTML))
var scope = tender.Scope{MerchantID: "consumer-demo", Environment: tender.EnvironmentMock}
var market = tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}

type order struct {
	ID, PaymentID, Owner string
	Total                tender.Money
}

// The fixture catalog models records returned by the host's order repository.
// Stable IDs let a restarted process load the same payment from PostgreSQL.
var orders = []order{
	{"5827a594-a4e9-4e24-ae20-211e764cfc00", "5c8f173c-6b94-48a7-8e48-b13bd941a826", "guest:demo", tender.Money{Minor: 2599, Currency: "USD"}},
	{"f7113464-3e89-4c5c-b3ce-1e05f838cd1a", "c46a25af-e13b-43f3-8615-52aa87ca008d", "account:demo", tender.Money{Minor: 4895, Currency: "USD"}},
}

type session struct {
	owner, csrf string
	expires     time.Time
}

type app struct {
	engine            *tender.Engine
	scope             tender.Scope
	market            tender.Market
	provider, account string
	card              *cardCheckout
	ledger            *sql.DB
	origin            string
	mu                sync.Mutex
	tickets           map[string]order
	sessions          map[string]session
}

type links struct{ Origin, Guest, Account string }

// Each optional MOCK checkout registers its own mounts during package initialization.
var mockCheckoutInstallers = map[string]func(*app, *http.ServeMux, tender.Store) (func(), error){}

func main() {
	addr := flag.String("listen", "127.0.0.1:8080", "loopback IP and port")
	env := flag.String("environment", "MOCK", "MOCK or SANDBOX (Adyen test merchant only)")
	flag.Parse()
	if err := run(*addr, *env); err != nil {
		log.Fatal(err)
	}
}

func run(addr, env string) error {
	provider := os.Getenv("TENDER_CONSUMER_PROVIDER")
	installMock := mockCheckoutInstallers[provider]
	if provider != "" && provider != "adyen" && installMock == nil {
		return errors.New("consumer: unknown checkout provider")
	}
	if env != string(tender.EnvironmentMock) && !(provider == "adyen" && env == string(tender.EnvironmentSandbox)) {
		return errors.New("consumer: checkout is available only for the MOCK fixture")
	}
	host, _, err := net.SplitHostPort(addr)
	ip, ipErr := netip.ParseAddr(host)
	if err != nil || ipErr != nil || !ip.IsLoopback() {
		return errors.New("consumer: listen address must be a loopback IP")
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	origin := "http://" + listener.Addr().String()
	if env == string(tender.EnvironmentSandbox) {
		origin = os.Getenv("TENDER_ADYEN_ORIGIN")
		if !sandboxOrigin(origin) {
			return errors.New("consumer: configure the registered HTTPS sandbox origin")
		}
	}
	var storage tender.Store = store.NewMemory()
	var db *sql.DB
	if dsn := os.Getenv("TENDER_CONSUMER_DATABASE_URL"); dsn != "" {
		db, err = openDB(dsn, os.Getenv("TENDER_CONSUMER_SCHEMA"))
		if err != nil {
			return err
		}
		defer db.Close()
		storage = postgres.New(db)
	}
	mux := http.NewServeMux()
	a := &app{scope: tender.Scope{MerchantID: scope.MerchantID, Environment: tender.Environment(env)}, market: market, ledger: db, origin: origin, sessions: make(map[string]session), tickets: make(map[string]order)}
	if provider == "adyen" {
		cleanup, err := a.installCardCheckout(mux, storage)
		if err != nil {
			return err
		}
		defer cleanup()
	} else if installMock != nil {
		cleanup, err := installMock(a, mux, storage)
		if err != nil {
			return err
		}
		defer cleanup()
	} else {
		sim, err := mockserver.New(mockserver.Config{PublicBaseURL: origin + "/simulator", Routes: fixtureRoutes})
		if err != nil {
			return err
		}
		defer sim.Close()
		a.provider, a.account = "synthetic", "fixture"
		a.engine, err = tender.NewEngine(tender.EngineConfig{Store: storage, Provider: fixture{sim}, Scope: a.scope, Market: a.market, ProviderName: a.provider, MerchantAccount: a.account})
		if err != nil {
			return err
		}
		mux.Handle("/simulator/", http.StripPrefix("/simulator", sim.Handler()))
		mux.HandleFunc("GET /orders/{order}", a.owned(a.view))
		mux.HandleFunc("POST /orders/{order}", a.owned(a.submit))
		mux.HandleFunc("POST /orders/{order}/confirm", a.owned(a.confirm))
	}
	// Restart recovery runs before browsing is possible. No client result can
	// trigger a delivery without a current payment and a committed outbox row.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, o := range orders {
		if err := a.project(ctx, o); err != nil {
			return fmt.Errorf("recover host projection: %w", err)
		}
	}
	guest, account := rand.Text(), rand.Text()
	a.tickets[guest], a.tickets[account] = orders[0], orders[1]
	mux.HandleFunc("GET /session/{ticket}", a.login)
	mux.HandleFunc("GET /orders/{order}/status", a.owned(a.status))
	publicURL, _ := url.Parse(origin)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		if r.Host != publicURL.Host {
			http.Error(w, "invalid host", 403)
			return
		}
		mux.ServeHTTP(w, r)
	})
	if err := json.NewEncoder(os.Stdout).Encode(links{origin, origin + "/session/" + guest, origin + "/session/" + account}); err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second}
	return server.Serve(listener)
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	// Go routes HEAD to GET by default. A link checker must not spend a ticket.
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	a.mu.Lock()
	o, ok := a.tickets[r.PathValue("ticket")]
	if !ok {
		a.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	delete(a.tickets, r.PathValue("ticket"))
	key := rand.Text()
	a.sessions[key] = session{o.Owner, rand.Text(), time.Now().Add(time.Hour)}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "demo_session", Value: key, Path: "/", HttpOnly: true, Secure: a.scope.Environment == tender.EnvironmentSandbox, SameSite: http.SameSiteStrictMode, MaxAge: 3600})
	http.Redirect(w, r, "/orders/"+o.ID, http.StatusSeeOther)
}

type orderHandler func(http.ResponseWriter, *http.Request, order, session)

func (a *app) owned(next orderHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("demo_session")
		if err != nil {
			http.Error(w, "sign in required", 401)
			return
		}
		a.mu.Lock()
		s, ok := a.sessions[c.Value]
		a.mu.Unlock()
		if !ok || !s.expires.After(time.Now()) {
			http.Error(w, "sign in required", 401)
			return
		}
		for _, o := range orders {
			if o.ID == r.PathValue("order") && o.Owner == s.owner {
				next(w, r, o, s)
				return
			}
		}
		http.NotFound(w, r)
	}
}

// Validate the exact browser contract before calling Tender. Never accept an
// order, payment ID, amount, currency or success assertion from the form.
func (a *app) form(w http.ResponseWriter, r *http.Request, s session, scenario bool) (url.Values, bool) {
	if r.Header.Get("Origin") != a.origin {
		http.Error(w, "origin rejected", 403)
		return nil, false
	}
	if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		http.Error(w, "invalid form", 400)
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return nil, false
	}
	f := r.PostForm
	if len(f["csrf"]) != 1 || subtle.ConstantTimeCompare([]byte(f.Get("csrf")), []byte(s.csrf)) != 1 {
		http.Error(w, "CSRF token rejected", 403)
		return nil, false
	}
	if scenario {
		if len(f) != 2 || len(f["scenario"]) != 1 || !knownScenario(f.Get("scenario")) {
			http.Error(w, "invalid scenario or fields", 400)
			return nil, false
		}
	} else if len(f) != 1 {
		http.Error(w, "unexpected fields", 400)
		return nil, false
	}
	return f, true
}

func (a *app) submit(w http.ResponseWriter, r *http.Request, o order, s session) {
	f, ok := a.form(w, r, s, true)
	if !ok {
		return
	}
	_, err := a.engine.Create(r.Context(), tender.Request{OrderID: o.ID, PaymentID: o.PaymentID, Key: "create/" + o.ID, Amount: o.Total, Token: f.Get("scenario")})
	a.finish(w, r, o, err)
}

func (a *app) confirm(w http.ResponseWriter, r *http.Request, o order, s session) {
	if _, ok := a.form(w, r, s, false); !ok {
		return
	}
	p, err := a.engine.Get(r.Context(), o.PaymentID)
	if err != nil && !errors.Is(err, tender.ErrNotFound) {
		a.finish(w, r, o, err)
		return
	}
	if err != nil || p.Amount != o.Total || p.OrderID != o.ID || (p.Status != tender.PaymentCaptured && !allowAction(p)) {
		http.Error(w, "synthetic action unavailable", 409)
		return
	}
	_, err = a.engine.Confirm(r.Context(), tender.Request{PaymentID: o.PaymentID, Key: "confirm/" + o.ID, Amount: o.Total, Token: "fixture-approved"})
	a.finish(w, r, o, err)
}

func allowAction(p tender.Payment) bool {
	return p.Status == tender.PaymentRequiresAction && p.Action.Mode == tender.ActionComponent && p.Action.Data == "fixture-step" && p.Action.Validate(time.Now()) == nil
}

func (a *app) finish(w http.ResponseWriter, r *http.Request, o order, err error) {
	if errors.Is(err, tender.ErrConflict) {
		http.Error(w, "checkout conflicts with the existing payment", 409)
		return
	}
	if err != nil {
		http.Error(w, "payment unavailable", 503)
		return
	}
	if err := a.project(r.Context(), o); err != nil {
		http.Error(w, "fulfillment pending", 503)
		return
	}
	http.Redirect(w, r, "/orders/"+o.ID, http.StatusSeeOther)
}

type orderStatus struct {
	OrderID, PaymentID, Currency, Status string
	Minor                                int64
	Funds                                *tender.Funds `json:",omitempty"`
	Fulfilled                            bool
	CaptureReceiptProcessed              bool `json:",omitempty"`
}

func (a *app) read(ctx context.Context, o order) (orderStatus, bool, error) {
	st := orderStatus{OrderID: o.ID, Currency: o.Total.Currency, Minor: o.Total.Minor, Status: "unpaid"}
	p, err := a.engine.Get(ctx, o.PaymentID)
	if errors.Is(err, tender.ErrNotFound) {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	if p.OrderID != o.ID || p.Amount != o.Total {
		return st, false, errors.New("order binding changed")
	}
	st.PaymentID, st.Status = p.ID, string(p.Status)
	if a.card != nil || mockCheckoutInstallers[a.provider] != nil || a.market.Method == tender.MethodACH {
		st.Funds = &p.Funds
	}
	if a.ledger != nil {
		err = a.ledger.QueryRowContext(ctx, `SELECT EXISTS (SELECT FROM consumer_fulfillments WHERE merchant_id=$1 AND environment=$2 AND order_id=$3 AND payment_id=$4)`, a.scope.MerchantID, a.scope.Environment, o.ID, p.ID).Scan(&st.Fulfilled)
	}
	if err == nil && a.provider == "paypal" && a.ledger != nil && p.ProviderReference != "" {
		err = a.ledger.QueryRowContext(ctx, `SELECT EXISTS (SELECT FROM tender_provider_events WHERE merchant_id=$1 AND environment=$2 AND provider='paypal' AND merchant_account=$3 AND processed_at IS NOT NULL AND evidence->>'OrderID'=$4 AND evidence->>'Kind'='PAYMENT.CAPTURE.COMPLETED')`, p.Scope.MerchantID, p.Scope.Environment, p.MerchantAccount, p.ProviderReference).Scan(&st.CaptureReceiptProcessed)
	}
	return st, allowAction(p), err
}

func (a *app) status(w http.ResponseWriter, r *http.Request, o order, _ session) {
	st, _, err := a.read(r.Context(), o)
	if err != nil {
		http.Error(w, "status unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(st)
}

func (a *app) view(w http.ResponseWriter, r *http.Request, o order, s session) {
	st, action, err := a.read(r.Context(), o)
	if err != nil {
		http.Error(w, "checkout unavailable", 503)
		return
	}
	// Keep the same-origin POST Origin available for the browser CSRF check.
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		orderStatus
		CSRF, Price string
		Action      bool
	}{st, s.csrf, fmt.Sprintf("$%d.%02d", st.Minor/100, st.Minor%100), action}
	w.Header().Set("X-CSRF-Token", s.csrf)
	if err := checkout.Execute(w, data); err != nil {
		log.Print("checkout response interrupted")
	}
}
