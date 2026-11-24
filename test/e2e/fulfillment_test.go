//go:build e2e && integration

package e2e_test

import (
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Exercise the pre-dial configuration step without opening a network connection.
func TestConsumerDatabaseRejectsRemoteBeforeDial(t *testing.T) {
	for _, tc := range []struct{ name, dsn string }{
		{"remote primary", "host=203.0.113.8 dbname=test sslmode=disable"},
		{"remote fallback", "host=127.0.0.1,db.example.invalid dbname=test sslmode=disable"},
		{"environment fallback", "dbname=test sslmode=disable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PGHOST", "localhost,203.0.113.9")
			cfg, err := localTestDatabaseConfig(tc.dsn)
			if cfg != nil || err == nil {
				t.Fatal("remote target produced a dialable configuration")
			}
		})
	}
}

func TestConsumerDatabasePinsLocalTargets(t *testing.T) {
	cfg, err := localTestDatabaseConfig("host=localhost,/tmp,localhost,::1 dbname=test sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" || len(cfg.Fallbacks) != 3 {
		t.Fatalf("primary %q, %d fallbacks", cfg.Host, len(cfg.Fallbacks))
	}
	for i, want := range []string{"/tmp", "127.0.0.1", "::1"} {
		if got := cfg.Fallbacks[i].Host; got != want {
			t.Errorf("fallback %d: got %q want %q", i, got, want)
		}
	}
}

// A committed collection survives failed host delivery. A fresh process must
// project it once and an exact checkout retry must preserve that one record.
func TestConsumerDurableFulfillment(t *testing.T) {
	dsn, schema, cfg := demoSchema(t, "ALTER TABLE consumer_fulfillments ADD CONSTRAINT hold_delivery CHECK (false)")
	ctx := t.Context()
	ident := pgx.Identifier{schema}.Sanitize()
	opts := []string{"TENDER_CONSUMER_DATABASE_URL=" + dsn, "TENDER_CONSUMER_SCHEMA=" + schema}
	var id string
	t.Run("committed payment, projection unavailable", func(t *testing.T) {
		d := launch(t, opts...)
		c := client(t, d.Origin)
		p := request(t, c, "GET", d.Guest, "", nil)
		target := d.Origin + p.path
		got := request(t, c, "POST", target, d.Origin, url.Values{"csrf": {token(t, p)}, "scenario": {"collected"}})
		if got.status != 503 {
			t.Fatalf("projection unavailable: %d", got.status)
		}
		s := status(t, c, target)
		if s.Status != "captured" || s.Fulfilled || s.Minor != 2599 {
			t.Fatalf("prior to recovery: %+v", s)
		}
		id = s.PaymentID
	})
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, "ALTER TABLE "+ident+".consumer_fulfillments DROP CONSTRAINT hold_delivery"); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	t.Run("restart and replay", func(t *testing.T) {
		d := launch(t, opts...)
		c := client(t, d.Origin)
		p := request(t, c, "GET", d.Guest, "", nil)
		target := d.Origin + p.path
		for range 2 {
			s := status(t, c, target)
			if !s.Fulfilled || s.PaymentID != id || s.Status != "captured" {
				t.Fatalf("after restart: %+v", s)
			}
			got := request(t, c, "POST", target, d.Origin, url.Values{"csrf": {token(t, p)}, "scenario": {"collected"}})
			if got.status != 200 {
				t.Fatalf("repeat: %d", got.status)
			}
		}
	})
	conn, err = pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var n, consumed int
	if err = conn.QueryRow(ctx, "SELECT (SELECT COUNT(*) FROM "+ident+".consumer_fulfillments), (SELECT COUNT(*) FROM "+ident+".tender_outbox WHERE consumed_at IS NOT NULL)").Scan(&n, &consumed); err != nil || n != 1 || consumed != 1 {
		t.Fatalf("fulfillments=%d consumed=%d: %v", n, consumed, err)
	}
}
