//go:build e2e

package e2e_test

import (
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/providers/adyen"
	"github.com/gravestench/tender/store"
)

// Exercise the mock consent surface with a real browser. Its buttons submit a
// fixture decision to the simulator. Only the opaque session result reaches checkout.
func TestACHCheckout(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	page := template.Must(template.New("checkout").Parse(`<!doctype html><title>Checkout</title><h1>Checkout</h1><p role="status">{{.Status}}</p>{{if .ID}}<section data-session="{{.ID}}"><p>Authorize a one-time synthetic USD 12.34 debit. No bank details are needed.</p><button value="approve">Approve test debit</button><button value="deny">Decline test debit</button></section><script src="/ach.js"></script>{{end}}`))
	mux.HandleFunc("GET /ach.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		io.WriteString(w, `document.querySelector('section').onclick=async e=>{if(e.target.tagName!=='BUTTON')return;const id=e.currentTarget.dataset.session;const receipt=await fetch('./sim/mock/consent/'+encodeURIComponent(id)+'/'+e.target.value,{method:'POST'});if(!receipt.ok)throw Error('consent');const {sessionResult}=await receipt.json();const result=await fetch('./result',{method:'POST',body:sessionResult});if(!result.ok)throw Error('result');document.querySelector('[role=status]').textContent=await result.text();document.querySelector('section').remove()}`)
	})
	for _, name := range []string{"approve", "deny", "validation-refusal", "sandbox", "live"} {
		environment := tender.EnvironmentMock
		origin := srv.URL
		base := srv.URL + "/" + name + "/sim"
		api := base + "/v72"
		switch name {
		case "sandbox":
			environment = tender.EnvironmentSandbox
			origin = "https://merchant.example"
			api = "https://checkout-test.adyen.com/v72"
		case "live":
			environment = tender.EnvironmentLive
			origin = "https://merchant.example"
			api = "https://merchant-checkout-live.adyenpayments.com/checkout/v72"
		}
		cfg := adyen.Config{Environment: environment, BaseURL: api, Origin: origin, ReturnURL: origin + "/checkout", APIKey: "mock-key", MerchantAccount: "merchant", LiveActivated: name == "live"}
		if environment == tender.EnvironmentMock {
			sim, err := mockserver.New(mockserver.Config{PublicBaseURL: base, Routes: mockserver.ACHRoutes(mockserver.ACHFixture{APIKey: cfg.APIKey, MerchantAccount: "merchant", ValidationRefused: name == "validation-refusal"})})
			if err != nil {
				t.Fatal(err)
			}
			defer sim.Close()
			prefix := "/" + name + "/sim"
			mux.Handle(prefix+"/", http.StripPrefix(prefix, sim.Handler()))
		} else {
			cfg.Transport = achNoNetwork{t}
		}
		a, err := adyen.NewACH(cfg)
		if err != nil {
			t.Fatal(err)
		}
		engine, err := tender.NewEngine(tender.EngineConfig{Provider: a, ProviderName: "adyen", MerchantAccount: "merchant", Store: store.NewMemory(), Scope: tender.Scope{Environment: environment, MerchantID: name}, Market: tender.Market{Country: "US", Currency: "USD", Method: tender.MethodACH}})
		if err != nil {
			t.Fatal(err)
		}
		amount := tender.Money{Minor: 1234, Currency: "USD"}
		_, err = engine.Create(t.Context(), tender.Request{PaymentID: "p", OrderID: "o", Key: "start", Amount: amount})
		if environment == tender.EnvironmentMock && err != nil || environment != tender.EnvironmentMock && !errors.Is(err, tender.ErrUnsupported) {
			t.Fatal(name, err)
		}
		textStatus := func(p tender.Payment) string {
			switch p.Status {
			case tender.PaymentPending:
				return "Debit pending"
			case tender.PaymentDeclined:
				return "Debit declined"
			case tender.PaymentRequiresAction:
				return "Awaiting test consent"
			}
			return "Bank payment unavailable"
		}
		mux.HandleFunc("GET /"+name+"/checkout", func(w http.ResponseWriter, r *http.Request) {
			p, _ := engine.Get(r.Context(), "p")
			view := struct{ Status, ID string }{Status: textStatus(p)}
			if action, err := a.CheckoutSession(p); err == nil {
				view.ID = action.ID
			}
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; form-action 'none'; base-uri 'none'")
			page.Execute(w, view)
		})
		mux.HandleFunc("POST /"+name+"/result", func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 100))
			if err != nil {
				w.WriteHeader(400)
				return
			}
			res, err := engine.Confirm(r.Context(), tender.Request{PaymentID: "p", Key: "result", Amount: amount, Token: string(raw)})
			if err != nil && !errors.Is(err, tender.ErrDeclined) || res.Payment.Funds != (tender.Funds{}) {
				w.WriteHeader(409)
				return
			}
			io.WriteString(w, textStatus(res.Payment))
		})
	}
	cmd := exec.CommandContext(t.Context(), "node", "-e", achBrowser, srv.URL)
	cmd.Env = os.Environ()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Chromium: %v\n%s", err, output)
	}
}

type achNoNetwork struct{ t *testing.T }

func (d achNoNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	d.t.Error("unavailable ACH attempted provider egress")
	return nil, tender.ErrUnsupported
}

const achBrowser = `
const assert=require('node:assert/strict'),{chromium}=require(process.env.TENDER_PLAYWRIGHT||'playwright');
(async()=>{const origin=process.argv[1], browser=await require('../../examples/consumer/browser-guard.cjs').launchBrowser(chromium,[origin]);
try{const ctx=await browser.newContext();const page=await ctx.newPage();let nav=0,extra=0;ctx.on('page',()=>extra++);page.on('framenavigated',f=>{if(f===page.mainFrame())nav++});
for(const [name,state] of [['approve','Debit pending'],['deny','Debit declined'],['validation-refusal','Debit declined'],['sandbox','Bank payment unavailable'],['live','Bank payment unavailable']]){
 const url=origin+'/'+name+'/checkout';await page.goto(url);const start=nav;assert.equal(await page.locator('input,form').count(),0);
 if(name!=='sandbox'&&name!=='live'){await page.getByRole('button',{name:name==='deny'?'Decline test debit':'Approve test debit'}).click()}
 await page.getByText(state,{exact:true}).waitFor();assert.equal(nav,start);await page.reload();await page.getByText(state,{exact:true}).waitFor();assert.equal(await page.getByRole('button').count(),0);assert.equal(page.url(),url);assert.equal(extra,0);
}}finally{await browser.close()}})().catch(e=>{console.error(e);process.exitCode=1});`
