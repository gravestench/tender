//go:build e2e

package e2e_test

import (
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/providers/adyen"
	"github.com/gravestench/tender/store"
)

// A real independent host process owns checkout, the signed inbox, settlement
// projection, fulfillment outbox and review ledger. Only opaque fixture consent
// results cross the browser boundary; no bank account fields enter the host.
func TestACHDurableReturn(t *testing.T) {
	for _, tc := range []struct {
		name, decision, fixture string
		returnFirst             bool
	}{
		{"consent denied", "deny", "", false},
		{"validation refused", "approve", "validation-refusal", false},
		{"bank pending", "approve", "pending", false},
		{"settled then returned", "approve", "", false},
		{"return before settlement", "approve", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, schema, cfg := demoSchema(t)
			opts := []string{"TENDER_CONSUMER_PROVIDER=adyen_ach", "TENDER_ACH_MOCK_RESULT=" + tc.fixture, "TENDER_CONSUMER_OPERATOR_KEY=" + operatorFixture, "TENDER_CONSUMER_DATABASE_URL=" + dsn, "TENDER_CONSUMER_SCHEMA=" + schema}
			var id string
			t.Run("host", func(t *testing.T) {
				d := launch(t, opts...)
				c := client(t, d.Origin)
				if got := request(t, c, "POST", d.Origin+"/simulator/v72/sessions", "", nil); got.status != 404 {
					t.Fatalf("shopper reached the private ACH fixture: %d", got.status)
				}
				p := request(t, c, "GET", d.Guest, "", nil)
				target := d.Origin + p.path
				form := url.Values{"csrf": {token(t, p)}}
				if got := request(t, c, "POST", target+"/ach/start", d.Origin, form); got.status != 200 {
					t.Fatalf("start ACH: %d %s", got.status, got.body)
				}
				if got := status(t, c, target); got.Status != "requires_action" || got.Fulfilled {
					t.Fatalf("consent pending: %+v", got)
				}
				if got := request(t, c, "POST", target+"/ach/consent/approve", d.Origin, url.Values{"csrf": {token(t, p)}, "accountNumber": {"synthetic-sentinel"}}); got.status != 400 {
					t.Fatalf("host accepted a bank account field: %d", got.status)
				}
				if got := request(t, c, "POST", target+"/ach/consent/"+tc.decision, d.Origin, form); got.status != 200 {
					t.Fatalf("consent: %d %s", got.status, got.body)
				}
				got := status(t, c, target)
				id = got.PaymentID
				for _, op := range []string{"void", "refund"} {
					if code := operatorRequest(t, c, d.Origin+"/operator/ach/orders/"+got.OrderID+"/"+op); code != 409 {
						t.Fatalf("unsupported ACH %s returned %d", op, code)
					}
				}
				if tc.decision == "deny" || tc.fixture == "validation-refusal" {
					if got.Status != "declined" || got.Fulfilled {
						t.Fatalf("failed consent collected: %+v", got)
					}
					return
				}
				if got.Status != "pending" || got.Fulfilled {
					t.Fatalf("bank debit must await settlement: %+v", got)
				}
				if tc.fixture == "pending" {
					return
				}
				settle := d.Origin + "/operator/ach/orders/" + got.OrderID + "/settle"
				returnURL := d.Origin + "/operator/ach/orders/" + got.OrderID + "/return"
				if got := request(t, c, "POST", settle, "", nil); got.status != 404 {
					t.Fatalf("shopper reached settlement projection: %d", got.status)
				}
				if tc.returnFirst {
					if code := operatorRequest(t, c, returnURL); code != 202 {
						t.Fatalf("early return: %d", code)
					}
					if s := status(t, c, target); s.Fulfilled {
						t.Fatalf("return before settlement fulfilled: %+v", s)
					}
				}
				if code := operatorRequest(t, c, settle); code != 202 {
					t.Fatalf("authenticated settlement: %d", code)
				}
				if tc.returnFirst {
					waitACH(t, c, target, "returned", false)
				} else {
					waitACH(t, c, target, "settled", true)
					for range 2 {
						if code := operatorRequest(t, c, returnURL); code != 202 {
							t.Fatalf("duplicate signed return: %d", code)
						}
					}
					waitACH(t, c, target, "returned", true)
				}
			})
			if t.Failed() || id == "" {
				return
			}
			conn := inspectDemo(t, cfg, schema)
			var deliveries, notices, returns, processed, consumed int
			var reviewKind, reviewOriginal, reviewModification string
			var reviewAmount int64
			err := conn.QueryRow(t.Context(), `SELECT
 (SELECT count(*) FROM consumer_fulfillments WHERE payment_id=$1),
 (SELECT count(*) FROM tender_adverse_reviews WHERE payment_id=$1),
 (SELECT count(*) FROM tender_provider_events WHERE event_id LIKE 'CHARGEBACK.%'),
 (SELECT count(*) FROM tender_provider_events WHERE processed_at IS NOT NULL),
 (SELECT count(*) FROM tender_outbox WHERE payment_id=$1 AND consumed_at IS NOT NULL)`, id).Scan(&deliveries, &notices, &returns, &processed, &consumed)
			if err == nil && notices == 1 {
				err = conn.QueryRow(t.Context(), `SELECT record->>'Kind',record->>'OriginalReference',record->>'ModificationReference',(record#>>'{Amount,Minor}')::bigint FROM tender_adverse_reviews WHERE payment_id=$1`, id).Scan(&reviewKind, &reviewOriginal, &reviewModification, &reviewAmount)
			}
			conn.Close(t.Context()) // PGlite serves one backend across the restart.
			if err != nil {
				t.Fatal(err)
			}
			if tc.decision == "deny" || tc.fixture == "validation-refusal" || tc.fixture == "pending" {
				if deliveries != 0 || notices != 0 {
					t.Fatalf("uncollected ACH created host effect: deliveries=%d reviews=%d", deliveries, notices)
				}
				return
			}
			wantDelivery := 1
			if tc.returnFirst {
				wantDelivery = 0
			}
			if deliveries != wantDelivery || consumed != wantDelivery || notices != 1 || returns != 1 || processed < 2 || reviewKind != "returned" || reviewOriginal == "" || reviewModification == "" || reviewOriginal == reviewModification || reviewAmount != 2599 {
				t.Fatalf("durable return: deliveries=%d consumed=%d reviews=%d return receipts=%d processed=%d kind=%s amount=%d", deliveries, consumed, notices, returns, processed, reviewKind, reviewAmount)
			}
			t.Run("restart", func(t *testing.T) {
				d := launch(t, opts...)
				c := client(t, d.Origin)
				p := request(t, c, "GET", d.Guest, "", nil)
				waitACH(t, c, d.Origin+p.path, "returned", wantDelivery == 1)
			})
			conn = inspectDemo(t, cfg, schema)
			var afterReviews, afterDeliveries int
			if err := conn.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM tender_adverse_reviews WHERE payment_id=$1),(SELECT count(*) FROM consumer_fulfillments WHERE payment_id=$1)`, id).Scan(&afterReviews, &afterDeliveries); err != nil || afterReviews != 1 || afterDeliveries != wantDelivery {
				t.Fatalf("restart duplicated compensation or delivery: reviews=%d deliveries=%d: %v", afterReviews, afterDeliveries, err)
			}
			conn.Close(t.Context())
		})
	}
}

func waitACH(t *testing.T, c *http.Client, target, state string, fulfilled bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		got := status(t, c, target)
		if got.Status == state && got.Fulfilled == fulfilled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ACH wanted %s fulfilled=%v, got %+v", state, fulfilled, got)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

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
