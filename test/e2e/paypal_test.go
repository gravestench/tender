//go:build e2e

package e2e_test

import (
	"context"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"os/exec"
	"testing"
)

func TestPayPalCheckout(t *testing.T) {
	for _, scenario := range []string{"component", "popup", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			dsn, schema, cfg := demoSchema(t)
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
				if payments != 1 || deliveries != 1 || events != 1 || operations != 2 {
					t.Fatalf("wallet database counts %d %d %d %d", payments, deliveries, events, operations)
				}
			} else if payments+deliveries+events+operations != 0 {
				t.Fatal("blocked presentation reserved or fulfilled a payment")
			}
		})
	}
}
