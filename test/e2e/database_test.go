//go:build e2e

package e2e_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/gravestench/tender/store/postgres"
	"github.com/jackc/pgx/v5"
)

func localTestDatabaseConfig(dsn string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid test database configuration")
	}
	// pgx expands PGHOST, service settings, and TLS fallback choices here.
	// Validate that effective list before exposing a config to any dialer.
	hosts := []*string{&cfg.Host}
	for _, fallback := range cfg.Fallbacks {
		hosts = append(hosts, &fallback.Host)
	}
	for _, host := range hosts {
		if strings.HasPrefix(*host, "/") && !strings.HasPrefix(*host, "//") {
			continue
		}
		if *host == "localhost" {
			*host = "127.0.0.1"
		}
		ip, err := netip.ParseAddr(*host)
		if err != nil || !ip.Unmap().IsLoopback() {
			return nil, errors.New("consumer E2E requires a local PostgreSQL primary and local fallback hosts")
		}
		*host = ip.Unmap().String()
	}
	return cfg, nil
}

func demoSchema(t *testing.T, hostSQL ...string) (string, string, *pgx.ConnConfig) {
	t.Helper()
	dsn := os.Getenv("TENDER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("set TENDER_TEST_DATABASE_URL to a disposable PostgreSQL database")
	}
	cfg, err := localTestDatabaseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	conn, err := pgx.ConnectConfig(ctx, cfg)
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
		conn, err := pgx.ConnectConfig(context.Background(), cfg)
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
	for _, sql := range append([]string{string(host)}, hostSQL...) {
		if _, err = conn.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if err = conn.Close(ctx); err != nil {
		t.Fatal(err)
	}
	return dsn, schema, cfg
}
