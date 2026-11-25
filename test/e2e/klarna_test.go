//go:build e2e

package e2e_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/providers/klarna"
	"github.com/gravestench/tender/store"
	"github.com/jackc/pgx/v5"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The production behavior under test is the host's decision to consume one
// fulfillment notice only after the complete, unrefunded provider capture.
func TestKlarnaDurableLifecycle(t *testing.T) {
	t.Run("partial capture and refund hold fulfillment", func(t *testing.T) {
		dsn, schema, cfg := demoSchema(t)
		operatorKey := rand.Text() + rand.Text()
		t.Run("checkout", func(t *testing.T) {
			d := launch(t, "TENDER_CONSUMER_PROVIDER=klarna", "TENDER_CONSUMER_DATABASE_URL="+dsn, "TENDER_CONSUMER_SCHEMA="+schema, "TENDER_KLARNA_MOCK_OPERATOR_TOKEN="+operatorKey)
			c := client(t, d.Origin)
			p := request(t, c, "GET", d.Guest, "", nil)
			base := d.Origin + p.path
			fields := url.Values{"csrf": {token(t, p)}}
			for _, action := range []string{"prepare", "approve"} {
				got := request(t, c, "POST", base+"/"+action, d.Origin, fields)
				if got.status != 200 {
					t.Fatalf("%s: %d %s", action, got.status, got.body)
				}
			}
			if st := status(t, c, base); st.Status != "authorized" || st.Fulfilled {
				t.Fatalf("approval fulfilled: %+v", st)
			}
			for _, step := range []struct {
				action, state      string
				captured, refunded int64
			}{
				{"capture-half", "partially_captured", 1299, 0},
				{"refund-half", "partially_captured", 1299, 1299},
				{"capture-rest", "captured", 2599, 1299},
			} {
				klarnaOperator(t, c, base, operatorKey, step.action, 204)
				st := klarnaStatus(t, c, base)
				if st.Status != step.state || st.Funds == nil || st.Funds.Captured != step.captured || st.Funds.Refunded != step.refunded || st.Fulfilled {
					t.Fatalf("%s: %+v", step.action, st)
				}
			}
			ref := klarnaOperator(t, c, base, operatorKey, "capture-rest", 204)
			klarnaProviderState(t, c, d.Origin, ref, 2599, 1299, "CAPTURED")
		})
		if t.Failed() {
			return
		}
		deliveries, consumed := klarnaLedgerCounts(t, cfg, schema)
		if deliveries != 0 || consumed != 0 {
			t.Fatalf("refunded payment produced delivery: %d %d", deliveries, consumed)
		}
	})
	for _, tc := range []struct {
		name, first, second, state string
		captured, delivered        int
	}{
		{"full capture exactly once", "capture-all", "capture-all", "captured", 2599, 1},
		{"cancel authorization", "cancel", "cancel", "voided", 0, 0},
		{"release after partial capture", "capture-half", "release", "voided", 1299, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, schema, cfg := demoSchema(t)
			operatorKey := rand.Text() + rand.Text()
			t.Run("checkout", func(t *testing.T) {
				d, c, base, fields := klarnaCheckout(t, dsn, schema, operatorKey)
				klarnaApprove(t, c, d.Origin, base, fields)
				klarnaOperator(t, c, base, "wrong", tc.first, 403)
				if st := status(t, c, base); st.Fulfilled {
					t.Fatal("unauthorized operator delivered order")
				}
				klarnaOperator(t, c, base, operatorKey, tc.first, 204)
				ref := klarnaOperator(t, c, base, operatorKey, tc.second, 204)
				providerStatus := "CANCELLED"
				if tc.delivered == 1 || tc.first == "capture-half" {
					providerStatus = "CAPTURED"
				}
				klarnaProviderState(t, c, d.Origin, ref, int64(tc.captured), 0, providerStatus)
				st := klarnaStatus(t, c, base)
				if st.Status != tc.state || st.Funds == nil || st.Funds.Captured != int64(tc.captured) || st.Fulfilled != (tc.delivered == 1) {
					t.Fatalf("%s: %+v", tc.name, st)
				}
			})
			if t.Failed() {
				return
			}
			fulfilled, consumed := klarnaLedgerCounts(t, cfg, schema)
			if fulfilled != tc.delivered || consumed != tc.delivered {
				t.Fatalf("%s: deliveries %d consumed %d", tc.name, fulfilled, consumed)
			}
		})
	}
	t.Run("pending fraud and ineligible product never collect", func(t *testing.T) {
		for _, tc := range []struct {
			name, env, state string
		}{
			{"fraud pending", "TENDER_KLARNA_MOCK_FRAUD_STATUS=PENDING", "pending"},
			{"pay now category", "TENDER_KLARNA_MOCK_CATEGORY=pay_now", "created"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dsn, schema, cfg := demoSchema(t)
				key := rand.Text() + rand.Text()
				t.Run("checkout", func(t *testing.T) {
					d, c, base, fields := klarnaCheckout(t, dsn, schema, key, tc.env)
					prepared := request(t, c, "POST", base+"/prepare", d.Origin, fields)
					if prepared.status != 200 {
						t.Fatalf("prepare: %d", prepared.status)
					}
					approved := request(t, c, "POST", base+"/approve", d.Origin, fields)
					want := 200
					if tc.state == "created" {
						want = 409
					}
					if approved.status != want {
						t.Fatalf("approval: got %d want %d", approved.status, want)
					}
					st := klarnaStatus(t, c, base)
					if st.Status != tc.state || st.Fulfilled || st.Funds == nil || st.Funds.Captured != 0 {
						t.Fatalf("blocked checkout: %+v", st)
					}
					klarnaOperator(t, c, base, key, "capture-all", 409)
				})
				if t.Failed() {
					return
				}
				if delivered, consumed := klarnaLedgerCounts(t, cfg, schema); delivered != 0 || consumed != 0 {
					t.Fatalf("blocked checkout delivered: %d %d", delivered, consumed)
				}
			})
		}
	})
	t.Run("expired and abandoned sessions remain unpaid", func(t *testing.T) {
		for _, expire := range []bool{false, true} {
			name := "abandoned"
			if expire {
				name = "expired"
			}
			t.Run(name, func(t *testing.T) {
				dsn, schema, cfg := demoSchema(t)
				key := rand.Text() + rand.Text()
				t.Run("checkout", func(t *testing.T) {
					d, c, base, fields := klarnaCheckout(t, dsn, schema, key)
					if got := request(t, c, "POST", base+"/prepare", d.Origin, fields); got.status != 200 {
						t.Fatalf("prepare: %d", got.status)
					}
					if expire {
						klarnaOperator(t, c, base, key, "advance-expiry", 204)
						if got := request(t, c, "POST", base+"/approve", d.Origin, fields); got.status != 409 {
							t.Fatalf("expired approval: %d", got.status)
						}
					}
					if st := klarnaStatus(t, c, base); st.Fulfilled || st.Funds == nil || st.Funds.Captured != 0 {
						t.Fatalf("inactive checkout: %+v", st)
					}
				})
				if t.Failed() {
					return
				}
				if delivered, consumed := klarnaLedgerCounts(t, cfg, schema); delivered != 0 || consumed != 0 {
					t.Fatalf("inactive checkout delivered: %d %d", delivered, consumed)
				}
			})
		}
	})
	t.Run("lost replies preserve uncertainty and capture recovery delivers once", func(t *testing.T) {
		for _, tc := range []struct{ name, fault, recover string }{
			{"order", "lose-order", ""},
			{"capture", "lose-capture", "recover-capture-all"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dsn, schema, cfg := demoSchema(t)
				key := rand.Text() + rand.Text()
				t.Run("checkout", func(t *testing.T) {
					d, c, base, fields := klarnaCheckout(t, dsn, schema, key)
					if got := request(t, c, "POST", base+"/prepare", d.Origin, fields); got.status != 200 {
						t.Fatalf("prepare: %d", got.status)
					}
					klarnaOperator(t, c, base, key, tc.fault, 204)
					if tc.name == "order" {
						if got := request(t, c, "POST", base+"/approve", d.Origin, fields); got.status != 200 {
							t.Fatalf("approval after lost order reply: %d", got.status)
						}
					} else {
						if got := request(t, c, "POST", base+"/approve", d.Origin, fields); got.status != 200 {
							t.Fatalf("approval: %d", got.status)
						}
						klarnaOperator(t, c, base, key, "capture-all", 409)
					}
					if st := status(t, c, base); st.Fulfilled {
						t.Fatal("lost reply fulfilled prematurely")
					}
					if tc.name == "order" {
						if st := klarnaStatus(t, c, base); st.Status != "requires_action" || st.Funds == nil || st.Funds.Captured != 0 {
							t.Fatalf("unknown order must remain uncollected: %+v", st)
						}
						klarnaOperator(t, c, base, key, "capture-all", 409)
						return
					}
					klarnaOperator(t, c, base, key, tc.recover, 204)
					klarnaOperator(t, c, base, key, tc.recover, 204)
					if st := klarnaStatus(t, c, base); st.Status != "captured" || !st.Fulfilled || st.Funds == nil || st.Funds.Captured != 2599 {
						t.Fatalf("recovered checkout: %+v", st)
					}
				})
				if t.Failed() {
					return
				}
				want := 1
				if tc.name == "order" {
					want = 0
				}
				if delivered, consumed := klarnaLedgerCounts(t, cfg, schema); delivered != want || consumed != want {
					t.Fatalf("recovered delivery: %d %d", delivered, consumed)
				}
				if tc.name == "order" && klarnaUnknownOperations(t, cfg, schema) != 1 {
					t.Fatal("lost order reply did not leave a durable unknown operation")
				}
			})
		}
	})
	t.Run("restart replays unconsumed host fulfillment", func(t *testing.T) {
		dsn, schema, cfg := demoSchema(t, `ALTER TABLE consumer_fulfillments ADD CONSTRAINT hold_klarna_delivery CHECK (false)`)
		key := rand.Text() + rand.Text()
		t.Run("before restart", func(t *testing.T) {
			d, c, base, fields := klarnaCheckout(t, dsn, schema, key)
			klarnaApprove(t, c, d.Origin, base, fields)
			klarnaOperator(t, c, base, key, "capture-all", 503)
			if st := klarnaStatus(t, c, base); st.Status != "captured" || st.Fulfilled {
				t.Fatalf("blocked delivery: %+v", st)
			}
		})
		if t.Failed() {
			return
		}
		conn, err := connectDemo(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = conn.Exec(t.Context(), "SET search_path TO "+pgx.Identifier{schema}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		if _, err = conn.Exec(t.Context(), "ALTER TABLE consumer_fulfillments DROP CONSTRAINT hold_klarna_delivery"); err != nil {
			t.Fatal(err)
		}
		conn.Close(context.Background())
		t.Run("after restart", func(t *testing.T) {
			d := launch(t, "TENDER_CONSUMER_PROVIDER=klarna", "TENDER_CONSUMER_DATABASE_URL="+dsn, "TENDER_CONSUMER_SCHEMA="+schema)
			c := client(t, d.Origin)
			p := request(t, c, "GET", d.Guest, "", nil)
			st := klarnaStatus(t, c, d.Origin+p.path)
			if st.Status != "captured" || !st.Fulfilled {
				t.Fatalf("restart projection: %+v", st)
			}
		})
		if t.Failed() {
			return
		}
		if delivered, consumed := klarnaLedgerCounts(t, cfg, schema); delivered != 1 || consumed != 1 {
			t.Fatalf("restart delivery: %d %d", delivered, consumed)
		}
	})
}

func klarnaCheckout(t *testing.T, dsn, schema, operatorKey string, extra ...string) (demo, *http.Client, string, url.Values) {
	t.Helper()
	d := launch(t, append([]string{"TENDER_CONSUMER_PROVIDER=klarna", "TENDER_CONSUMER_DATABASE_URL=" + dsn, "TENDER_CONSUMER_SCHEMA=" + schema, "TENDER_KLARNA_MOCK_OPERATOR_TOKEN=" + operatorKey}, extra...)...)
	c := client(t, d.Origin)
	p := request(t, c, "GET", d.Guest, "", nil)
	if p.status != 200 {
		t.Fatalf("Klarna checkout: %d", p.status)
	}
	return d, c, d.Origin + p.path, url.Values{"csrf": {token(t, p)}}
}

func klarnaApprove(t *testing.T, c *http.Client, origin, base string, fields url.Values) {
	t.Helper()
	for _, action := range []string{"prepare", "approve"} {
		got := request(t, c, "POST", base+"/"+action, origin, fields)
		if got.status != 200 {
			t.Fatalf("%s: %d %s", action, got.status, got.body)
		}
	}
}

func klarnaLedgerCounts(t *testing.T, cfg *pgx.ConnConfig, schema string) (int, int) {
	t.Helper()
	conn, err := connectDemo(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err = conn.Exec(t.Context(), "SET search_path TO "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	var delivered, consumed int
	if err = conn.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM consumer_fulfillments), (SELECT count(*) FROM tender_outbox WHERE consumed_at IS NOT NULL)`).Scan(&delivered, &consumed); err != nil {
		t.Fatal(err)
	}
	return delivered, consumed
}

func klarnaUnknownOperations(t *testing.T, cfg *pgx.ConnConfig, schema string) int {
	t.Helper()
	conn, err := connectDemo(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err = conn.Exec(t.Context(), "SET search_path TO "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = conn.QueryRow(t.Context(), `SELECT count(*) FROM tender_operations WHERE status='unknown'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func klarnaStatus(t *testing.T, c *http.Client, target string) struct {
	Status, PaymentID string
	Funds             *tender.Funds
	Fulfilled         bool
} {
	t.Helper()
	p := request(t, c, "GET", target+"/status", "", nil)
	var st struct {
		Status, PaymentID string
		Funds             *tender.Funds
		Fulfilled         bool
	}
	if p.status != 200 || json.Unmarshal([]byte(p.body), &st) != nil {
		t.Fatalf("Klarna status: %d %s", p.status, p.body)
	}
	return st
}

func klarnaOperator(t *testing.T, c *http.Client, target, key, action string, want int) string {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), "POST", u.Scheme+"://"+u.Host+"/fixture/klarna"+u.Path+"/"+action, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tender-Mock-Operator", key)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != want {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("operator %s: got %d want %d: %s", action, res.StatusCode, want, body)
	}
	return res.Header.Get("X-Tender-Mock-Provider-Order")
}

func klarnaProviderState(t *testing.T, c *http.Client, origin, reference string, captured, refunded int64, state string) {
	t.Helper()
	if reference == "" {
		t.Fatal("operator did not identify provider order")
	}
	req, err := http.NewRequestWithContext(t.Context(), "GET", origin+"/simulator/ordermanagement/v1/orders/"+reference, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Basic mock-only-klarna")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var provider struct {
		OrderID  string `json:"order_id"`
		Captured int64  `json:"captured_amount"`
		Refunded int64  `json:"refunded_amount"`
		Status   string `json:"status"`
	}
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&provider) != nil || provider.OrderID != reference || provider.Captured != captured || provider.Refunded != refunded || provider.Status != state {
		t.Fatalf("provider state: HTTP %d %+v", res.StatusCode, provider)
	}
}

// This host exercises the provider package over a real loopback listener.
// Its synthetic approval button stands in for Klarna's external widget.
func TestKlarnaCheckout(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	type buyer struct {
		adapter *klarna.Adapter
		engine  *tender.Engine
		request tender.Request
	}
	buyers := map[string]buyer{}
	var elapsed atomic.Int64
	baseline := time.Now().UTC()
	for _, name := range []string{"approved", "expired", "ineligible", "strict"} {
		scope := tender.Scope{MerchantID: name, Environment: tender.EnvironmentMock}
		db := store.NewMemory()
		cfg := klarna.Config{Scope: scope, Store: db, BaseURL: srv.URL + "/" + name + "/sim", APIKey: "mock-key", MerchantAccount: "mid", CallbackURL: srv.URL + "/" + name + "/callback", CallbackKey: []byte("0123456789abcdef0123456789abcdef"), Now: func() time.Time { return baseline.Add(time.Duration(elapsed.Load())) }, OrderLines: func(context.Context, tender.Payment) ([]klarna.Line, error) {
			return []klarna.Line{{Type: "digital", Name: "Course", Quantity: 1, UnitPrice: 100, TotalAmount: 100}}, nil
		}}
		if name == "strict" {
			scope.Environment = tender.EnvironmentSandbox
			cfg.Scope = scope
			cfg.BaseURL = klarna.PlaygroundURL
			cfg.APIKey = "klarna_test_api_fixture"
			cfg.CallbackURL = "https://shop.example/callback"
			cfg.Transport = denyKlarnaNetwork{t}
		} else {
			fx := mockserver.KlarnaFixture{MerchantAccount: "mid", APIKey: cfg.APIKey, CallbackURL: cfg.CallbackURL}
			if name == "ineligible" {
				fx.Category = "pay_now"
			}
			sim, err := mockserver.New(mockserver.Config{PublicBaseURL: cfg.BaseURL, Routes: mockserver.KlarnaRoutes(fx)})
			if err != nil {
				t.Fatal(err)
			}
			defer sim.Close()
			mux.Handle("/"+name+"/sim/", http.StripPrefix("/"+name+"/sim", sim.Handler()))
		}
		a, err := klarna.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		mux.Handle("/"+name+"/callback", a.CallbackHandler())
		e, err := tender.NewEngine(tender.EngineConfig{Scope: scope, Store: db, Provider: a, ProviderName: "klarna", MerchantAccount: "mid", Market: tender.Market{Country: "US", Currency: "USD", Method: tender.MethodKlarna}, Now: cfg.Now})
		if err != nil {
			t.Fatal(err)
		}
		req := tender.Request{PaymentID: "payment", OrderID: "order", Amount: tender.Money{Currency: "USD", Minor: 100}, Key: "start"}
		if _, err = e.Create(t.Context(), req); err != nil && !errors.Is(err, tender.ErrUnsupported) {
			t.Fatal(err)
		}
		buyers[name] = buyer{a, e, req}
	}
	page := template.Must(template.New("checkout").Parse(`<!doctype html><title>Checkout</title><p role="status">{{.Status}}</p>{{if .Session}}<button id="approve" data-session="{{.Session}}">Approve synthetic purchase</button><script src="./checkout.js"></script>{{end}}`))
	mux.HandleFunc("GET /{buyer}/checkout", func(w http.ResponseWriter, r *http.Request) {
		b := buyers[r.PathValue("buyer")]
		if b.engine == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
		view := struct{ Status, Session string }{Status: "Financing unavailable"}
		p, err := b.engine.Get(r.Context(), b.request.PaymentID)
		if err == nil {
			if action, err := b.adapter.CheckoutAction(p); err == nil {
				view.Status = "Synthetic financing"
				view.Session = action.SessionID
			} else if p.Status == tender.PaymentAuthorized {
				view.Status = "Authorized"
			}
		}
		w.Header().Set("Content-Type", "text/html")
		_ = page.Execute(w, view)
	})
	mux.HandleFunc("GET /{buyer}/checkout.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		fmt.Fprint(w, `document.querySelector('button').onclick=async e=>{const first=await fetch('./sim/mock/authorize/'+encodeURIComponent(e.currentTarget.dataset.session),{method:'POST'});if(!first.ok)throw Error('approval');const second=await fetch('./process',{method:'POST'});if(!second.ok)throw Error('order');document.querySelector('[role=status]').textContent='Authorized';document.querySelector('button').remove()}`)
	})
	mux.HandleFunc("POST /{buyer}/process", func(w http.ResponseWriter, r *http.Request) {
		b := buyers[r.PathValue("buyer")]
		if b.engine == nil {
			http.NotFound(w, r)
			return
		}
		ids, err := b.adapter.Authorizations(r.Context(), "payment")
		if err != nil || len(ids) != 1 {
			w.WriteHeader(409)
			return
		}
		res, err := b.adapter.ProcessAuthorization(r.Context(), b.engine, "payment", ids[0])
		if err != nil || res.Payment.Status != tender.PaymentAuthorized {
			w.WriteHeader(409)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /clock", func(w http.ResponseWriter, _ *http.Request) { elapsed.Store(int64(49 * time.Hour)); w.WriteHeader(204) })
	cmd := exec.CommandContext(t.Context(), "node", "-e", klarnaBrowser)
	cmd.Env = append(os.Environ(), "TENDER_KLARNA_TEST_ORIGIN="+srv.URL)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Klarna browser: %v\n%s", err, output)
	}
	for name, b := range buyers {
		p, err := b.engine.Get(t.Context(), "payment")
		if name == "approved" {
			if err != nil || p.Status != tender.PaymentAuthorized || p.Funds.Captured != 0 {
				t.Fatal("bad authorized order", p, err)
			}
		} else if err == nil && p.Funds != (tender.Funds{}) {
			t.Fatal("unavailable checkout moved funds", name, p)
		}
	}
}

type denyKlarnaNetwork struct{ t *testing.T }

func (d denyKlarnaNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	d.t.Error("strict checkout attempted external network access")
	return nil, tender.ErrUnsupported
}

const klarnaBrowser = `
const assert = require("node:assert/strict");
const {chromium} = require(process.env.TENDER_PLAYWRIGHT || "playwright");
(async () => {
 const origin=process.env.TENDER_KLARNA_TEST_ORIGIN;
 const browser=await require("../../examples/consumer/browser-guard.cjs").launchBrowser(chromium,[origin]);
 try{
   const ctx=await browser.newContext({serviceWorkers:"block"});
   const outside=[],popups=[],navigations=[];
   await ctx.route("**/*",route=>{if(new URL(route.request().url()).origin===origin)return route.continue();outside.push(route.request().url());return route.abort()});
   await ctx.routeWebSocket("**/*",route=>route.close());
   const page=await ctx.newPage();
   ctx.on("page",p=>popups.push(p.url()));
   page.on("framenavigated",f=>{if(f===page.mainFrame())navigations.push(f.url())});
   for(const name of ["strict","ineligible","approved","expired"]){
     const target=origin+"/"+name+"/checkout";await page.goto(target);
     const count=navigations.length;
     if(name==="approved"){await page.getByRole("button").click();await page.getByText("Authorized",{exact:true}).waitFor();assert.equal(navigations.length,count)}
     else if(name==="expired"){await page.getByRole("button").waitFor();await page.evaluate(()=>fetch("/clock",{method:"POST"}))}
     await page.reload();
     await page.getByRole("status").getByText(name==="approved"?"Authorized":"Financing unavailable",{exact:true}).waitFor();
     assert.equal(await page.getByRole("button").count(),0);
     assert.equal(page.url(),target);assert.equal(navigations.length,count+1);
   }
   assert.deepEqual(outside,[]);assert.deepEqual(popups,[]);assert.equal(ctx.pages().length,1);
 }finally{await browser.close()}
})().catch(err=>{console.error(err);process.exitCode=1});`

func TestKlarnaConsumerCheckout(t *testing.T) {
	dsn, schema, _ := demoSchema(t)
	d := launch(t, "TENDER_CONSUMER_PROVIDER=klarna", "TENDER_CONSUMER_DATABASE_URL="+dsn, "TENDER_CONSUMER_SCHEMA="+schema)
	c := client(t, d.Origin)
	p := request(t, c, "GET", d.Guest, "", nil)
	if p.status != 200 || !strings.Contains(p.body, "Start synthetic financing") {
		t.Fatal("Klarna checkout missing", p.status)
	}
	target := d.Origin + p.path
	fields := url.Values{"csrf": {token(t, p)}}
	next := request(t, c, "POST", target+"/prepare", d.Origin, fields)
	if next.status != 200 || !strings.Contains(next.body, "Approve synthetic purchase") {
		t.Fatal("session unavailable", next.status)
	}
	approved := request(t, c, "POST", target+"/approve", d.Origin, fields)
	if approved.status != 200 || !strings.Contains(approved.body, "authorized") {
		t.Fatal("order unavailable", approved.status)
	}
	s := status(t, c, target)
	if s.Status != "authorized" || s.Minor != 2599 || s.Fulfilled {
		t.Fatalf("host authorization = %+v", s)
	}
	// The account ticket remains fresh for a real browser run of the
	// independent module, including its shipped checkout JavaScript.
	cmd := exec.CommandContext(t.Context(), "node", "-e", `
const assert=require("node:assert/strict");
const {chromium}=require(process.env.TENDER_PLAYWRIGHT||"playwright");
(async()=>{const origin=process.env.TENDER_ORIGIN;
 const browser=await require("../../examples/consumer/browser-guard.cjs").launchBrowser(chromium,[origin]);
 try{
   const ctx=await browser.newContext({serviceWorkers:"block"});const page=await ctx.newPage();
   const windows=[],navigations=[];ctx.on("page",p=>windows.push(p.url()));
   await page.goto(process.env.TENDER_TICKET);const checkout=page.url();
   page.on("framenavigated",frame=>{if(frame===page.mainFrame())navigations.push(frame.url())});
   await page.getByRole("button",{name:"Start synthetic financing"}).click();
   await page.getByRole("button",{name:"Approve synthetic purchase"}).click();
   await page.getByText("authorized",{exact:true}).waitFor();
   assert.deepEqual(windows,[]);assert.deepEqual(navigations,[]);assert.equal(page.url(),checkout);
   await page.reload();await page.getByText("authorized",{exact:true}).waitFor();
   assert.equal(await page.getByRole("button").count(),0);
   const state=await page.evaluate(async()=>await(await fetch(location.pathname+"/status")).json());
   assert.equal(state.Status,"authorized");assert.equal(state.Fulfilled,false);assert.equal(state.Minor,4895)
 }finally{await browser.close()}
})().catch(err=>{console.error(err);process.exitCode=1});`)
	cmd.Env = append(os.Environ(), "TENDER_ORIGIN="+d.Origin, "TENDER_TICKET="+d.Account)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("independent Klarna browser: %v\n%s", err, out)
	}
	refreshed := request(t, c, "GET", target, "", nil)
	if refreshed.status != 200 || strings.Contains(refreshed.body, "Approve synthetic purchase") {
		t.Fatal("approval returned on refresh")
	}
}
