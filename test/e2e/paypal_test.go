//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestPayPalCheckout(t *testing.T) {
	for _, scenario := range []string{"component", "popup", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			// Force the callback worker to retry until the browser has finished.
			// Capture and fulfillment alone are insufficient evidence of inbox drain.
			dsn, schema, cfg := demoSchema(t, `ALTER TABLE tender_provider_events ADD CONSTRAINT hold_wallet_receipt CHECK (processed_at IS NULL OR processed_at >= received_at + interval '2 seconds')`)
			t.Run("browser", func(t *testing.T) {
				d := launch(t, "TENDER_CONSUMER_PROVIDER=paypal", "TENDER_PAYPAL_MOCK_PRESENTATION="+scenario, "TENDER_CONSUMER_DATABASE_URL="+dsn, "TENDER_CONSUMER_SCHEMA="+schema)
				cmd := exec.CommandContext(t.Context(), "node", "../../examples/consumer/mock-paypal-browser.cjs")
				cmd.Env = append(os.Environ(), "TENDER_ORIGIN="+d.Origin, "TENDER_TICKET="+d.Guest, "TENDER_VIEW="+scenario)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("wallet browser: %s %v", out, err)
				}
				c := client(t, d.Origin)
				page := request(t, c, "GET", d.Account, "", nil)
				if page.status != 200 {
					t.Fatal("account page", page.status)
				}
				if got := request(t, c, "POST", d.Origin+page.path+"/wallet/start", d.Origin, url.Values{"csrf": {token(t, page)}, "amount": {"1"}}); got.status != 400 {
					t.Fatal("client price accepted", got.status)
				}
				if scenario == "component" {
					fields := url.Values{"csrf": {token(t, page)}}
					base := d.Origin + page.path
					for _, step := range []string{"start", "confirm", "approve"} {
						expected := 200
						if step == "confirm" {
							expected = 409
						}
						if r := request(t, c, "POST", base+"/wallet/"+step, d.Origin, fields); r.status != expected {
							t.Fatalf("interrupted checkout %s = %d", step, r.status)
						}
					}
					// A browser can disconnect after approval and before its confirm POST.
					if st := status(t, c, base); st.Status == "captured" || st.Fulfilled {
						t.Fatal("buyer approval alone collected funds")
					}
					for _, step := range []string{"approve", "confirm"} {
						if r := request(t, c, "POST", base+"/wallet/"+step, d.Origin, fields); r.status != 200 {
							t.Fatalf("resumed checkout %s = %d", step, r.status)
						}
					}
					if st := status(t, c, base); st.Status != "captured" || !st.Fulfilled {
						t.Fatal("resumed wallet did not collect once")
					}
					deadline := time.Now().Add(12 * time.Second)
					for {
						var state struct{ CaptureReceiptProcessed bool }
						reply := request(t, c, "GET", base+"/status", "", nil)
						if reply.status != 200 || json.Unmarshal([]byte(reply.body), &state) != nil {
							t.Fatal("unreadable wallet receipt status")
						}
						if state.CaptureReceiptProcessed {
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("timed out waiting for durable wallet capture receipt")
						}
						time.Sleep(25 * time.Millisecond)
					}

				}
				if scenario != "component" {
					if got := request(t, c, "POST", d.Origin+page.path+"/wallet/start", d.Origin, url.Values{"csrf": {token(t, page)}}); got.status != 409 {
						t.Fatal("blocked start submitted", got.status)
					}
				}
			})
			if t.Failed() {
				return
			}
			conn, err := connectDemo(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(context.Background())
			if _, err = conn.Exec(t.Context(), "SET search_path TO "+pgx.Identifier{schema}.Sanitize()); err != nil {
				t.Fatal(err)
			}
			var payments, deliveries, events, operations int
			err = conn.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM tender_payments), (SELECT count(*) FROM consumer_fulfillments), (SELECT count(*) FROM tender_provider_events WHERE processed_at IS NOT NULL), (SELECT count(*) FROM tender_operations)`).Scan(&payments, &deliveries, &events, &operations)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "component" {
				if payments != 2 || deliveries != 2 || events != 2 || operations != 5 {
					t.Fatalf("wallet database counts %d %d %d %d", payments, deliveries, events, operations)
				}
			} else if payments+deliveries+events+operations != 0 {
				t.Fatal("blocked presentation reserved or fulfilled a payment")
			}
		})
	}
}
