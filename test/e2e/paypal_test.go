//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const walletOperatorFixture = "local-paypal-operator-credential-only"

// Exercise the host checkout, the provider fixture and the durable store.
func TestPayPalDurableLifecycle(t *testing.T) {
	t.Run("pending capture completes once after a verified transition and replay", func(t *testing.T) {
		dsn, schema, cfg := demoSchema(t)
		var paymentID string
		t.Run("host", func(t *testing.T) {
			d := launch(t, "TENDER_CONSUMER_PROVIDER=paypal", "TENDER_PAYPAL_MOCK_PRESENTATION=component", "TENDER_PAYPAL_OPERATOR_KEY="+walletOperatorFixture, "TENDER_CONSUMER_DATABASE_URL="+dsn, "TENDER_CONSUMER_SCHEMA="+schema)
			guest, other := client(t, d.Origin), client(t, d.Origin)
			p := request(t, guest, "GET", d.Guest, "", nil)
			q := request(t, other, "GET", d.Account, "", nil)
			base := d.Origin + p.path
			fields := url.Values{"csrf": {token(t, p)}}
			otherBase := d.Origin + q.path
			otherFields := url.Values{"csrf": {token(t, q)}}
			for _, step := range []string{"start", "approve"} {
				if got := request(t, guest, "POST", base+"/wallet/"+step, d.Origin, fields); got.status != 200 {
					t.Fatalf("%s: %d %s", step, got.status, got.body)
				}
				if got := request(t, other, "POST", otherBase+"/wallet/"+step, d.Origin, otherFields); got.status != 200 {
					t.Fatalf("other %s: %d %s", step, got.status, got.body)
				}
			}
			orderID := status(t, guest, base).OrderID
			if got := walletFixtureTransition(t, guest, d.Origin, orderID, "capture-next", "PENDING", ""); got != 404 {
				t.Fatalf("unauthenticated fixture control: %d", got)
			}
			if got := walletFixtureTransition(t, guest, d.Origin, orderID, "capture-next", "PENDING", walletOperatorFixture); got != 204 {
				t.Fatalf("pending fixture setup: %d", got)
			}
			if got := request(t, other, "POST", otherBase+"/wallet/confirm", d.Origin, otherFields); got.status != 200 {
				t.Fatalf("other confirm after targeted pending setup: %d %s", got.status, got.body)
			}
			if got := status(t, other, otherBase); got.Status != "captured" || !got.Fulfilled {
				t.Fatalf("pending setup changed another order: %+v", got)
			}
			if got := request(t, guest, "POST", base+"/wallet/confirm", d.Origin, fields); got.status != 200 {
				t.Fatalf("pending confirm: %d %s", got.status, got.body)
			}
			pending := status(t, guest, base)
			paymentID = pending.PaymentID
			if pending.Status != "pending" || pending.Fulfilled {
				t.Fatalf("pending fulfilled an order: %+v", pending)
			}
			provider := walletProviderState(t, guest, d.Origin, orderID)
			if len(provider.Units) != 1 || len(provider.Units[0].Payments.Captures) != 1 || provider.Units[0].Payments.Captures[0].Status != "PENDING" {
				t.Fatalf("provider pending capture: %+v", provider)
			}
			if got := walletFixtureTransition(t, guest, d.Origin, orderID, "capture-status", "COMPLETED", walletOperatorFixture); got != 204 {
				t.Fatalf("complete pending capture: %d", got)
			}
			deadline := time.Now().Add(12 * time.Second)
			for {
				completed := status(t, guest, base)
				if completed.Status == "captured" && completed.Fulfilled {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("verified completion did not fulfill: %+v", completed)
				}
				time.Sleep(25 * time.Millisecond)
			}
			for range 2 {
				if got := walletFixtureTransition(t, guest, d.Origin, orderID, "capture-status", "COMPLETED", walletOperatorFixture); got != 204 {
					t.Fatalf("replayed completion: %d", got)
				}
			}
			if got := status(t, other, otherBase); got.Status != "captured" || !got.Fulfilled {
				t.Fatalf("other order changed: %+v", got)
			}
		})
		if t.Failed() {
			return
		}
		paypalResetPGlite(t, cfg)
		t.Run("restart", func(t *testing.T) {
			d := launch(t, "TENDER_CONSUMER_PROVIDER=paypal", "TENDER_PAYPAL_MOCK_PRESENTATION=component", "TENDER_CONSUMER_DATABASE_URL="+dsn, "TENDER_CONSUMER_SCHEMA="+schema)
			c := client(t, d.Origin)
			p := request(t, c, "GET", d.Guest, "", nil)
			got := status(t, c, d.Origin+p.path)
			if got.PaymentID != paymentID || got.Status != "captured" || !got.Fulfilled {
				t.Fatalf("restart lost completion: %+v", got)
			}
		})
		if t.Failed() {
			return
		}
		conn := paypalInspect(t, cfg, schema)
		var captures, events, outbox, fulfilled, other int
		err := conn.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM tender_operations WHERE payment_id=$1 AND record->>'Kind'='confirm' AND status='succeeded'), (SELECT count(*) FROM tender_provider_events WHERE provider='paypal' AND processed_at IS NOT NULL), (SELECT count(*) FROM tender_outbox WHERE payment_id=$1 AND consumed_at IS NOT NULL), (SELECT count(*) FROM consumer_fulfillments WHERE payment_id=$1), (SELECT count(*) FROM consumer_fulfillments WHERE payment_id<>$1)`, paymentID).Scan(&captures, &events, &outbox, &fulfilled, &other)
		if err != nil {
			t.Fatal(err)
		}
		if captures != 1 || events != 3 || outbox != 1 || fulfilled != 1 || other != 1 {
			t.Fatalf("pending completion counts: captures=%d events=%d outbox=%d fulfilled=%d other=%d", captures, events, outbox, fulfilled, other)
		}
	})
	t.Run("denied capture never fulfills and survives restart", func(t *testing.T) {
		dsn, schema, cfg := demoSchema(t)
		var paymentID string
		t.Run("host", func(t *testing.T) {
			d := launch(t, "TENDER_CONSUMER_PROVIDER=paypal", "TENDER_PAYPAL_MOCK_PRESENTATION=component", "TENDER_PAYPAL_OPERATOR_KEY="+walletOperatorFixture, "TENDER_CONSUMER_DATABASE_URL="+dsn, "TENDER_CONSUMER_SCHEMA="+schema)
			c := client(t, d.Origin)
			p := request(t, c, "GET", d.Guest, "", nil)
			base := d.Origin + p.path
			fields := url.Values{"csrf": {token(t, p)}}
			for _, step := range []string{"start", "approve"} {
				if got := request(t, c, "POST", base+"/wallet/"+step, d.Origin, fields); got.status != 200 {
					t.Fatalf("%s: %d %s", step, got.status, got.body)
				}
			}
			orderID := status(t, c, base).OrderID
			if got := walletFixtureTransition(t, c, d.Origin, orderID, "capture-next", "DENIED", walletOperatorFixture); got != 204 {
				t.Fatalf("denied fixture setup: %d", got)
			}
			if got := request(t, c, "POST", base+"/wallet/confirm", d.Origin, fields); got.status != 409 {
				t.Fatalf("denied confirm: %d %s", got.status, got.body)
			}
			got := status(t, c, base)
			paymentID = got.PaymentID
			if got.Status != "declined" || got.Fulfilled {
				t.Fatalf("denied capture fulfilled: %+v", got)
			}
			provider := walletProviderState(t, c, d.Origin, orderID)
			if len(provider.Units) != 1 || len(provider.Units[0].Payments.Captures) != 1 || provider.Units[0].Payments.Captures[0].Status != "DECLINED" {
				t.Fatalf("provider denied capture: %+v", provider)
			}
			if got := walletFixtureTransition(t, c, d.Origin, orderID, "capture-status", "DENIED", walletOperatorFixture); got != 204 {
				t.Fatalf("replayed denial: %d", got)
			}
		})
		if t.Failed() {
			return
		}
		paypalResetPGlite(t, cfg)
		t.Run("restart", func(t *testing.T) {
			d := launch(t, "TENDER_CONSUMER_PROVIDER=paypal", "TENDER_PAYPAL_MOCK_PRESENTATION=component", "TENDER_CONSUMER_DATABASE_URL="+dsn, "TENDER_CONSUMER_SCHEMA="+schema)
			c := client(t, d.Origin)
			p := request(t, c, "GET", d.Guest, "", nil)
			got := status(t, c, d.Origin+p.path)
			if got.PaymentID != paymentID || got.Status != "declined" || got.Fulfilled {
				t.Fatalf("restart changed denial: %+v", got)
			}
		})
		if t.Failed() {
			return
		}
		conn := paypalInspect(t, cfg, schema)
		var failed, events, outbox, fulfilled int
		err := conn.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM tender_operations WHERE payment_id=$1 AND record->>'Kind'='confirm' AND status='failed'), (SELECT count(*) FROM tender_provider_events WHERE provider='paypal' AND processed_at IS NOT NULL), (SELECT count(*) FROM tender_outbox WHERE payment_id=$1), (SELECT count(*) FROM consumer_fulfillments WHERE payment_id=$1)`, paymentID).Scan(&failed, &events, &outbox, &fulfilled)
		if err != nil {
			t.Fatal(err)
		}
		if failed != 1 || events != 1 || outbox != 0 || fulfilled != 0 {
			t.Fatalf("denied capture counts: failed=%d events=%d outbox=%d fulfilled=%d", failed, events, outbox, fulfilled)
		}
	})
	t.Run("cancel reload and abandoned approval never collect funds", func(t *testing.T) {
		dsn, schema, cfg := demoSchema(t)
		var paymentID string
		t.Run("host", func(t *testing.T) {
			d := launch(t, "TENDER_CONSUMER_PROVIDER=paypal", "TENDER_PAYPAL_MOCK_PRESENTATION=component", "TENDER_PAYPAL_OPERATOR_KEY="+walletOperatorFixture, "TENDER_CONSUMER_DATABASE_URL="+dsn, "TENDER_CONSUMER_SCHEMA="+schema)
			c := client(t, d.Origin)
			p := request(t, c, "GET", d.Guest, "", nil)
			base := d.Origin + p.path
			fields := url.Values{"csrf": {token(t, p)}}
			if got := request(t, c, "POST", base+"/wallet/start", d.Origin, fields); got.status != 200 {
				t.Fatalf("start: %d", got.status)
			}
			if got := request(t, c, "POST", base+"/wallet/cancel", d.Origin, fields); got.status != 200 || !strings.Contains(got.body, "cancelled") {
				t.Fatalf("cancel: %d %s", got.status, got.body)
			}
			for range 2 {
				if got := status(t, c, base); got.Status != "requires_action" || got.Fulfilled {
					t.Fatalf("cancel moved funds: %+v", got)
				} else {
					paymentID = got.PaymentID
				}
				if page := request(t, c, "GET", base, "", nil); page.status != 200 || !strings.Contains(page.body, "Synthetic PayPal approval") {
					t.Fatalf("refresh lost resumable action: %d", page.status)
				}
			}
			if got := request(t, c, "POST", base+"/wallet/approve", d.Origin, fields); got.status != 200 {
				t.Fatalf("approve: %d", got.status)
			}
			if got := status(t, c, base); got.Status == "captured" || got.Fulfilled {
				t.Fatalf("abandoned approval collected: %+v", got)
			}
		})
		if t.Failed() {
			return
		}
		t.Run("restart after abandonment", func(t *testing.T) {
			d := launch(t, "TENDER_CONSUMER_PROVIDER=paypal", "TENDER_PAYPAL_MOCK_PRESENTATION=component", "TENDER_CONSUMER_DATABASE_URL="+dsn, "TENDER_CONSUMER_SCHEMA="+schema)
			c := client(t, d.Origin)
			p := request(t, c, "GET", d.Guest, "", nil)
			if p.status != 200 || !strings.Contains(p.body, "PayPal is unavailable for this checkout") || strings.Contains(p.body, "Approve synthetic payment") {
				t.Fatalf("restart exposed vanished fixture action: %d %s", p.status, p.body)
			}
			base := d.Origin + p.path
			if got := request(t, c, "POST", base+"/wallet/approve", d.Origin, url.Values{"csrf": {token(t, p)}}); got.status != 409 {
				t.Fatalf("restart accepted vanished approval: %d", got.status)
			}
			if got := status(t, c, base); got.PaymentID != paymentID || got.Fulfilled {
				t.Fatalf("restart changed abandoned payment: %+v", got)
			}
		})
		if t.Failed() {
			return
		}
		conn := paypalInspect(t, cfg, schema)
		var captures, deliveries, outbox int
		if err := conn.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM tender_operations WHERE payment_id=$1 AND record->>'Kind'='confirm'), (SELECT count(*) FROM consumer_fulfillments WHERE payment_id=$1), (SELECT count(*) FROM tender_outbox WHERE payment_id=$1)`, paymentID).Scan(&captures, &deliveries, &outbox); err != nil {
			t.Fatal(err)
		}
		if captures+deliveries+outbox != 0 {
			t.Fatalf("abandoned approval persisted collection: %d %d %d", captures, deliveries, outbox)
		}
	})
	t.Run("duplicate confirmation captures and fulfills once after restart", func(t *testing.T) {
		dsn, schema, cfg := demoSchema(t)
		var paymentID string
		t.Run("capture", func(t *testing.T) {
			d := launch(t, "TENDER_CONSUMER_PROVIDER=paypal", "TENDER_PAYPAL_MOCK_PRESENTATION=component", "TENDER_PAYPAL_OPERATOR_KEY="+walletOperatorFixture, "TENDER_CONSUMER_DATABASE_URL="+dsn, "TENDER_CONSUMER_SCHEMA="+schema)
			c := client(t, d.Origin)
			p := request(t, c, "GET", d.Guest, "", nil)
			base := d.Origin + p.path
			fields := url.Values{"csrf": {token(t, p)}}
			for _, step := range []string{"start", "approve"} {
				if got := request(t, c, "POST", base+"/wallet/"+step, d.Origin, fields); got.status != 200 {
					t.Fatalf("%s: %d", step, got.status)
				}
			}
			var wg sync.WaitGroup
			codes := make(chan int, 2)
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					req, err := http.NewRequestWithContext(t.Context(), "POST", base+"/wallet/confirm", strings.NewReader(fields.Encode()))
					if err != nil {
						codes <- 0
						return
					}
					req.Header.Set("Origin", d.Origin)
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					res, err := c.Do(req)
					if err != nil {
						codes <- 0
						return
					}
					res.Body.Close()
					codes <- res.StatusCode
				}()
			}
			wg.Wait()
			close(codes)
			accepted := 0
			for code := range codes {
				if code == 200 {
					accepted++
				} else if code != 409 {
					t.Fatalf("duplicate confirm: %d", code)
				}
			}
			if accepted == 0 {
				t.Fatal("neither confirmation captured")
			}
			got := status(t, c, base)
			if got.Status != "captured" || !got.Fulfilled {
				t.Fatalf("duplicate capture: %+v", got)
			}
			paymentID = got.PaymentID
			provider := walletProviderState(t, c, d.Origin, got.OrderID)
			if provider.Status != "COMPLETED" || len(provider.Units) != 1 || len(provider.Units[0].Payments.Captures) != 1 || provider.Units[0].Payments.Captures[0].Status != "COMPLETED" {
				t.Fatalf("provider captured more than once: %+v", provider)
			}
		})
		if t.Failed() {
			return
		}
		t.Run("restart", func(t *testing.T) {
			d := launch(t, "TENDER_CONSUMER_PROVIDER=paypal", "TENDER_PAYPAL_MOCK_PRESENTATION=component", "TENDER_CONSUMER_DATABASE_URL="+dsn, "TENDER_CONSUMER_SCHEMA="+schema)
			c := client(t, d.Origin)
			p := request(t, c, "GET", d.Guest, "", nil)
			got := status(t, c, d.Origin+p.path)
			if got.PaymentID != paymentID || got.Status != "captured" || !got.Fulfilled {
				t.Fatalf("restart: %+v", got)
			}
		})
		if t.Failed() {
			return
		}
		conn := paypalInspect(t, cfg, schema)
		var captures, deliveries, consumed int
		if err := conn.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM tender_operations WHERE payment_id=$1 AND record->>'Kind'='confirm' AND status='succeeded'), (SELECT count(*) FROM consumer_fulfillments WHERE payment_id=$1), (SELECT count(*) FROM tender_outbox WHERE payment_id=$1 AND consumed_at IS NOT NULL)`, paymentID).Scan(&captures, &deliveries, &consumed); err != nil {
			t.Fatal(err)
		}
		if captures != 1 || deliveries != 1 || consumed != 1 {
			t.Fatalf("duplicate durable counts: %d %d %d", captures, deliveries, consumed)
		}
	})
	t.Run("lost capture reply recovers from verified receipt", func(t *testing.T) {
		dsn, schema, cfg := demoSchema(t)
		var paymentID string
		t.Run("host", func(t *testing.T) {
			d := launch(t, "TENDER_CONSUMER_PROVIDER=paypal", "TENDER_PAYPAL_MOCK_PRESENTATION=component", "TENDER_PAYPAL_MOCK_LOST_RESPONSE=capture", "TENDER_CONSUMER_DATABASE_URL="+dsn, "TENDER_CONSUMER_SCHEMA="+schema)
			c := client(t, d.Origin)
			p := request(t, c, "GET", d.Guest, "", nil)
			base := d.Origin + p.path
			fields := url.Values{"csrf": {token(t, p)}}
			for _, step := range []string{"start", "approve"} {
				if got := request(t, c, "POST", base+"/wallet/"+step, d.Origin, fields); got.status != 200 {
					t.Fatalf("%s: %d", step, got.status)
				}
			}
			if got := request(t, c, "POST", base+"/wallet/confirm", d.Origin, fields); got.status != 409 {
				t.Fatalf("lost reply: %d %s", got.status, got.body)
			}
			paymentID = status(t, c, base).PaymentID
			deadline := time.Now().Add(12 * time.Second)
			for {
				got := status(t, c, base)
				if got.Status == "captured" && got.Fulfilled {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("lost capture did not recover: %+v", got)
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
		if t.Failed() {
			return
		}
		conn := paypalInspect(t, cfg, schema)
		var captures, events, deliveries int
		if err := conn.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM tender_operations WHERE payment_id=$1 AND record->>'Kind'='confirm' AND status='succeeded'), (SELECT count(*) FROM tender_provider_events WHERE processed_at IS NOT NULL), (SELECT count(*) FROM consumer_fulfillments WHERE payment_id=$1)`, paymentID).Scan(&captures, &events, &deliveries); err != nil {
			t.Fatal(err)
		}
		if captures != 1 || events != 1 || deliveries != 1 {
			t.Fatalf("lost reply counts: %d %d %d", captures, events, deliveries)
		}
	})
	t.Run("partial refund is host controlled and durable", func(t *testing.T) {
		dsn, schema, cfg := demoSchema(t)
		var paymentID string
		t.Run("host", func(t *testing.T) {
			d := launch(t, "TENDER_CONSUMER_PROVIDER=paypal", "TENDER_PAYPAL_MOCK_PRESENTATION=component", "TENDER_PAYPAL_OPERATOR_KEY="+walletOperatorFixture, "TENDER_CONSUMER_DATABASE_URL="+dsn, "TENDER_CONSUMER_SCHEMA="+schema)
			c := client(t, d.Origin)
			p := request(t, c, "GET", d.Guest, "", nil)
			base := d.Origin + p.path
			fields := url.Values{"csrf": {token(t, p)}}
			for _, step := range []string{"start", "approve", "confirm"} {
				got := request(t, c, "POST", base+"/wallet/"+step, d.Origin, fields)
				if got.status != 200 {
					t.Fatalf("%s: %d %s", step, got.status, got.body)
				}
			}
			before := status(t, c, base)
			if before.Status != "captured" || !before.Fulfilled {
				t.Fatalf("capture: %+v", before)
			}
			paymentID = before.PaymentID
			endpoint := d.Origin + "/operator/wallet/orders/" + before.OrderID + "/refund"
			if got := request(t, c, "POST", endpoint, d.Origin, nil); got.status != 404 {
				t.Fatalf("buyer refund access: %d", got.status)
			}
			for range 2 {
				req, err := http.NewRequestWithContext(t.Context(), "POST", endpoint, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+walletOperatorFixture)
				res, err := c.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				if res.StatusCode != 202 {
					t.Fatalf("host refund: %d", res.StatusCode)
				}
			}
			after := request(t, c, "GET", base+"/status", "", nil)
			if after.status != 200 || !json.Valid([]byte(after.body)) || !containsRefund(after.body, 1000) {
				t.Fatalf("partial refund: %d %s", after.status, after.body)
			}
			if !status(t, c, base).Fulfilled {
				t.Fatal("refund erased prior fulfillment")
			}
			provider := walletProviderState(t, c, d.Origin, before.OrderID)
			if provider.Status != "COMPLETED" || len(provider.Units) != 1 || len(provider.Units[0].Payments.Captures) != 1 || provider.Units[0].Payments.Captures[0].Status != "PARTIALLY_REFUNDED" {
				t.Fatalf("provider partial refund: %+v", provider)
			}
		})
		if t.Failed() {
			return
		}
		conn := paypalInspect(t, cfg, schema)
		var refunds, fulfillment, consumed int
		err := conn.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM tender_operations WHERE payment_id=$1 AND record->>'Kind'='refund' AND status='succeeded'), (SELECT count(*) FROM consumer_fulfillments WHERE payment_id=$1), (SELECT count(*) FROM tender_outbox WHERE payment_id=$1 AND consumed_at IS NOT NULL)`, paymentID).Scan(&refunds, &fulfillment, &consumed)
		if err != nil {
			t.Fatal(err)
		}
		if refunds != 1 || fulfillment != 1 || consumed != 1 {
			t.Fatalf("durable refund counts: %d %d %d", refunds, fulfillment, consumed)
		}
	})
}

func paypalInspect(t *testing.T, cfg *pgx.ConnConfig, schema string) *pgx.Conn {
	t.Helper()
	conn, err := connectDemo(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if _, err := conn.Exec(t.Context(), "SET search_path TO "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	return conn
}

func paypalResetPGlite(t *testing.T, cfg *pgx.ConnConfig) {
	t.Helper()
	if os.Getenv("TENDER_TEST_PGLITE") != "1" {
		return
	}
	conn, err := connectDemo(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

type walletFixtureOrder struct {
	Status string `json:"status"`
	Units  []struct {
		Payments struct {
			Captures []struct{ ID, Status string } `json:"captures"`
		} `json:"payments"`
	} `json:"purchase_units"`
}

func walletProviderState(t *testing.T, c *http.Client, origin, orderID string) walletFixtureOrder {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "GET", origin+"/operator/wallet/orders/"+orderID+"/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+walletOperatorFixture)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var state walletFixtureOrder
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&state) != nil {
		t.Fatalf("provider fixture: %d", res.StatusCode)
	}
	return state
}

func walletFixtureTransition(t *testing.T, c *http.Client, origin, orderID, action, state, key string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "POST", origin+"/operator/wallet/orders/"+orderID+"/"+action+"/"+state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func containsRefund(raw string, want int64) bool {
	var state struct{ Funds struct{ Refunded int64 } }
	return json.Unmarshal([]byte(raw), &state) == nil && state.Funds.Refunded == want
}

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
