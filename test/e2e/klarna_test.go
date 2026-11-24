//go:build e2e

package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/providers/klarna"
	"github.com/gravestench/tender/store"
	"html/template"
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
