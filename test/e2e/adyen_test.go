//go:build e2e

package e2e_test

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const operatorFixture = "local-test-operator-credential-only"

func TestAdyenCheckout(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, brand, operation string
		browser, lose                   bool
	}{
		{"Visa purchase, lost capture and refund", "Authorised", "visa", "refund", false, true},
		{"Mastercard direct challenge and cancel", "ChallengeShopper", "mc", "void", false, false},
		{"decline", "Refused", "visa", "", false, false},
		{"redirect stops in checkout", "RedirectShopper", "visa", "", true, false},
		{"unknown interaction stops in checkout", "UnknownAction", "mc", "", true, false},
		{"fingerprint with SDK fallback stops in checkout", "IdentifyShopper", "visa", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, schema, cfg := demoSchema(t)
			opts := []string{"TENDER_CONSUMER_PROVIDER=adyen", "TENDER_ADYEN_MOCK_RESULT=" + tc.outcome, "TENDER_ADYEN_BRAND=" + tc.brand, "TENDER_CONSUMER_OPERATOR_KEY=" + operatorFixture, "TENDER_CONSUMER_DATABASE_URL=" + dsn, "TENDER_CONSUMER_SCHEMA=" + schema}
			if tc.lose {
				opts = append(opts, "TENDER_ADYEN_MOCK_LOST_RESPONSE=captures")
			}
			var id string
			t.Run("host", func(t *testing.T) {
				d := launch(t, opts...)
				if tc.browser {
					cmd := exec.CommandContext(t.Context(), "node", "../../examples/consumer/mock-card-browser.cjs")
					cmd.Env = append(os.Environ(), "TENDER_ORIGIN="+d.Origin, "TENDER_TICKET="+d.Account, "TENDER_VIEW="+tc.outcome)
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("checkout browser: %s: %v", out, err)
					}
				}
				c := client(t, d.Origin)
				p := request(t, c, "GET", d.Guest, "", nil)
				target := d.Origin + p.path
				fields := url.Values{"csrf": {token(t, p)}}
				if got := request(t, c, "POST", target, d.Origin, url.Values{"csrf": {token(t, p)}, "amount": {"1"}}); got.status != 400 {
					t.Fatalf("browser price override: %d", got.status)
				}
				p = request(t, c, "POST", target, d.Origin, fields)
				if p.status != 200 || !strings.Contains(p.body, "Use synthetic card") {
					t.Fatalf("expected Adyen mock card control, got HTTP %d", p.status)
				}
				id = status(t, c, target).PaymentID
				p = request(t, c, "GET", target, "", nil)
				if !strings.Contains(p.body, "Use synthetic card") || status(t, c, target).PaymentID != id {
					t.Fatal("refresh lost the active checkout")
				}
				initialConfirm := d.Origin + confirmationPath(t, p)
				p = request(t, c, "POST", initialConfirm, d.Origin, fields)
				if p.status != 200 {
					t.Fatalf("confirm: %d", p.status)
				}
				switch tc.outcome {
				case "RedirectShopper", "UnknownAction", "IdentifyShopper":
					if p.path != strings.TrimPrefix(target, d.Origin) || !strings.Contains(p.body, "This payment needs review") || strings.Contains(p.body, "issuer.invalid") || strings.Contains(p.body, "/confirm") {
						t.Fatal("unsupported action reached checkout")
					}
					if next := request(t, c, "POST", initialConfirm, d.Origin, fields); next.status != 409 {
						t.Fatalf("blocked action re-submitted: %d", next.status)
					}
					return
				case "Refused":
					if got := status(t, c, target); got.Status != "declined" || got.Fulfilled {
						t.Fatalf("decline: %+v", got)
					}
					return
				case "ChallengeShopper":
					if !strings.Contains(p.body, "Complete synthetic challenge") {
						t.Fatal("direct challenge missing")
					}
					again := request(t, c, "GET", target, "", nil)
					if !strings.Contains(again.body, "Complete synthetic challenge") {
						t.Fatal("challenge lost on refresh")
					}
					if stale := request(t, c, "POST", initialConfirm, d.Origin, fields); stale.status != 409 {
						t.Fatalf("stale card submit advanced the challenge: %d", stale.status)
					}
					p = request(t, c, "POST", d.Origin+confirmationPath(t, again), d.Origin, fields)
					if p.status != 200 {
						t.Fatalf("challenge details: %d", p.status)
					}
				}
				got := status(t, c, target)
				if got.Status != "authorized" || got.Fulfilled || got.Minor != 2599 {
					t.Fatalf("authorization: %+v", got)
				}
				opURL := d.Origin + "/operator/orders/" + got.OrderID + "/"
				if got := request(t, c, "POST", opURL+"capture", d.Origin, nil); got.status != 404 {
					t.Fatalf("shopper can operate on funds: %d", got.status)
				}
				if tc.operation == "void" {
					if code := operatorRequest(t, c, opURL+"void"); code != 202 {
						t.Fatalf("cancel: %d", code)
					}
					waitCard(t, c, target, "voided", false)
					return
				}
				code := operatorRequest(t, c, opURL+"capture")
				if code != 202 && !(tc.lose && code == 409) {
					t.Fatalf("capture: %d", code)
				}
				waitCard(t, c, target, "captured", true)
				if code := operatorRequest(t, c, opURL+"capture"); code != 202 {
					t.Fatalf("replay capture: %d", code)
				}
				if code := operatorRequest(t, c, opURL+"refund"); code != 202 {
					t.Fatalf("refund: %d", code)
				}
				deadline := time.Now().Add(8 * time.Second)
				for {
					p := request(t, c, "GET", target+"/status", "", nil)
					if strings.Contains(p.body, `"Refunded":2599`) {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("verified refund not applied")
					}
					time.Sleep(25 * time.Millisecond)
				}
			})
			if t.Failed() {
				return
			}
			conn := inspectDemo(t, cfg, schema)
			var fulfilled, consumed, events int
			err := conn.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM consumer_fulfillments WHERE payment_id=$1),(SELECT count(*) FROM tender_outbox WHERE payment_id=$1 AND consumed_at IS NOT NULL),(SELECT count(*) FROM tender_provider_events WHERE processed_at IS NOT NULL)`, id).Scan(&fulfilled, &consumed, &events)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.operation {
			case "refund":
				if fulfilled != 1 || consumed != 1 || events != 3 {
					t.Fatalf("after duplicate delivery and replay: deliveries=%d outbox=%d events=%d", fulfilled, consumed, events)
				}
			case "void":
				if fulfilled != 0 || consumed != 0 || events != 2 {
					t.Fatalf("void: deliveries=%d outbox=%d events=%d", fulfilled, consumed, events)
				}
			default:
				if fulfilled != 0 || consumed != 0 {
					t.Fatal("unexpected fulfillment")
				}
			}
		})
	}
	t.Run("lost authorization stops input pending brand proof", func(t *testing.T) {
		dsn, schema, _ := demoSchema(t)
		d := launch(t, "TENDER_CONSUMER_PROVIDER=adyen", "TENDER_ADYEN_MOCK_LOST_RESPONSE=payments", "TENDER_CONSUMER_DATABASE_URL="+dsn, "TENDER_CONSUMER_SCHEMA="+schema)
		c := client(t, d.Origin)
		p := request(t, c, "GET", d.Guest, "", nil)
		target := d.Origin + p.path
		fields := url.Values{"csrf": {token(t, p)}}
		prepared := request(t, c, "POST", target, d.Origin, fields)
		post := d.Origin + confirmationPath(t, prepared)
		res := request(t, c, "POST", post, d.Origin, fields)
		if res.status != 503 {
			t.Fatalf("expected unknown authorization, got HTTP %d", res.status)
		}
		again := request(t, c, "GET", target, "", nil)
		if !strings.Contains(again.body, "This payment needs review") || strings.Contains(again.body, "Use synthetic card") || status(t, c, target).Fulfilled {
			t.Fatal("unresolved authorization exposed a fresh card action")
		}
		if replay := request(t, c, "POST", post, d.Origin, fields); replay.status != 409 {
			t.Fatalf("unresolved payment accepted another input: %d", replay.status)
		}
	})
	t.Run("abandoned checkout expires across restart", func(t *testing.T) {
		dsn, schema, cfg := demoSchema(t)
		opts := []string{"TENDER_CONSUMER_PROVIDER=adyen", "TENDER_CONSUMER_DATABASE_URL=" + dsn, "TENDER_CONSUMER_SCHEMA=" + schema}
		var id, initialConfirmPath string
		t.Run("abandon", func(t *testing.T) {
			d := launch(t, opts...)
			c := client(t, d.Origin)
			p := request(t, c, "GET", d.Guest, "", nil)
			target := d.Origin + p.path
			prepared := request(t, c, "POST", target, d.Origin, url.Values{"csrf": {token(t, p)}})
			initialConfirmPath = confirmationPath(t, prepared)
			got := status(t, c, target)
			id = got.PaymentID
			if got.Fulfilled || got.Status != "requires_action" {
				t.Fatalf("abandonment: %+v", got)
			}
		})
		t.Run("advance persisted deadline", func(t *testing.T) {
			conn := inspectDemo(t, cfg, schema)
			res, err := conn.Exec(t.Context(), `UPDATE tender_payments SET record=jsonb_set(record,'{Action,ExpiresAt}',to_jsonb('2001-01-01T00:00:00Z'::text),false) WHERE payment_id=$1`, id)
			if err != nil || res.RowsAffected() != 1 {
				t.Fatalf("expiry setup: %v", err)
			}
		})
		t.Run("restore", func(t *testing.T) {
			d := launch(t, opts...)
			c := client(t, d.Origin)
			p := request(t, c, "GET", d.Guest, "", nil)
			target := d.Origin + p.path
			if !strings.Contains(p.body, "This payment needs review") || strings.Contains(p.body, "Use synthetic card") {
				t.Fatal("expired action exposed on refresh")
			}
			if got := request(t, c, "POST", d.Origin+initialConfirmPath, d.Origin, url.Values{"csrf": {token(t, p)}}); got.status != 409 {
				t.Fatalf("accepted expired action: %d", got.status)
			}
			if got := status(t, c, target); got.Fulfilled || got.PaymentID != id {
				t.Fatal("abandonment changed payment identity or fulfillment")
			}
		})
	})
}

func inspectDemo(t *testing.T, cfg *pgx.ConnConfig, schema string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.ConnectConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if _, err := conn.Exec(t.Context(), "SET search_path TO "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	return conn
}
func operatorRequest(t *testing.T, c *http.Client, target string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "POST", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+operatorFixture)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}
func waitCard(t *testing.T, c *http.Client, target, state string, fulfilled bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := status(t, c, target)
		if got.Status == state && got.Fulfilled == fulfilled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("wanted %s, fulfillment %v, got %+v", state, fulfilled, got)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

var confirmPathRE = regexp.MustCompile(`<form method="post" action="(/orders/[^"]+/confirm(?:/\d+)?)"`)

func confirmationPath(t *testing.T, p page) string {
	t.Helper()
	m := confirmPathRE.FindStringSubmatch(p.body)
	if len(m) != 2 {
		t.Fatal("no current confirmation form")
	}
	return m[1]
}
