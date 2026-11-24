package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/netip"
	"path/filepath"

	"github.com/gravestench/tender"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func openDB(dsn, schema string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("consumer: invalid database configuration")
	}
	// Check pgx's entire failover list before opening a MOCK pool. Pin
	// localhost to loopback so the fixture never needs a DNS lookup.
	hosts := []*string{&cfg.Host}
	for _, fallback := range cfg.Fallbacks {
		hosts = append(hosts, &fallback.Host)
	}
	for _, host := range hosts {
		if *host == "localhost" {
			*host = "127.0.0.1"
		}
		if filepath.IsAbs(*host) {
			continue
		}
		ip, err := netip.ParseAddr(*host)
		if err != nil || !ip.Unmap().IsLoopback() {
			return nil, errors.New("consumer: MOCK database requires a loopback IP or Unix socket")
		}
	}
	if schema == "" {
		return nil, errors.New("consumer: set TENDER_CONSUMER_SCHEMA to the migrated schema")
	}
	db := stdlib.OpenDB(*cfg, stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO "+pgx.Identifier{schema}.Sanitize())
		return err
	}))
	return db, nil
}

// project is the host's fulfillment policy for this single-merchant card
// fixture. The ledger entry is the whole example fulfillment effect. An
// external shipment would require an idempotent destination or another outbox.
func (a *app) project(ctx context.Context, o order) error {
	if a.ledger == nil {
		return nil
	}
	tx, err := a.ledger.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var seq int64
	var entryID string
	var raw []byte
	err = tx.QueryRowContext(ctx, `
SELECT n.seq, n.entry_id, p.record
FROM tender_outbox n JOIN tender_payments p USING (merchant_id, environment, payment_id)
WHERE n.merchant_id=$1 AND n.environment=$2 AND n.payment_id=$3
  AND p.order_id=$4 AND n.record->>'OrderID'=$4 AND n.consumed_at IS NULL
FOR UPDATE OF n,p`, scope.MerchantID, scope.Environment, o.PaymentID, o.ID).Scan(&seq, &entryID, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var p tender.Payment
	if err = json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if p.Market != market || p.Provider != "synthetic" || p.MerchantAccount != "fixture" || p.Amount != o.Total || p.Status != tender.PaymentCaptured ||
		p.Funds.Captured != o.Total.Minor || p.Funds.Refunded != 0 || p.Funds.Returned != 0 || p.Funds.Disputed != 0 {
		return nil // Hold the notice for host review after any conflicting facts.
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO consumer_fulfillments (merchant_id,environment,order_id,payment_id,entry_id) VALUES ($1,$2,$3,$4,$5)`, scope.MerchantID, scope.Environment, o.ID, o.PaymentID, entryID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tender_outbox SET consumed_at=CURRENT_TIMESTAMP WHERE seq=$1`, seq); err != nil {
		return err
	}
	return tx.Commit()
}
