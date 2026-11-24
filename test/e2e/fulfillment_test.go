//go:build e2e && integration

package e2e_test

import (
	"context"
	"crypto/rand"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/gravestench/tender/store/postgres"
	"github.com/jackc/pgx/v5"
)

// A committed collection survives failed host delivery. A fresh process must
// project it once and an exact checkout retry must preserve that one record.
func TestConsumerDurableFulfillment(t *testing.T) {
	dsn := os.Getenv("TENDER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("set TENDER_TEST_DATABASE_URL to a disposable PostgreSQL database")
	}
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	schema := "consumer_e2e_" + strings.ToLower(rand.Text())
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err = conn.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close(context.Background())
		if _, err = conn.Exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	if _, err = conn.Exec(ctx, "SET search_path TO "+ident); err != nil {
		t.Fatal(err)
	}
	names, err := fs.Glob(postgres.Migrations, "migrations/*.up.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("find Tender migrations: %v", err)
	}
	for _, name := range names {
		migration, err := postgres.Migrations.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = conn.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	host, err := os.ReadFile("../../examples/consumer/fulfillment.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{string(host), "ALTER TABLE consumer_fulfillments ADD CONSTRAINT hold_delivery CHECK (false)"} {
		if _, err = conn.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if err = conn.Close(ctx); err != nil {
		t.Fatal(err)
	}
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
	conn, err = pgx.Connect(ctx, dsn)
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
	conn, err = pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var n, consumed int
	if err = conn.QueryRow(ctx, "SELECT (SELECT COUNT(*) FROM "+ident+".consumer_fulfillments), (SELECT COUNT(*) FROM "+ident+".tender_outbox WHERE consumed_at IS NOT NULL)").Scan(&n, &consumed); err != nil || n != 1 || consumed != 1 {
		t.Fatalf("fulfillments=%d consumed=%d: %v", n, consumed, err)
	}
}
