//go:build integration

package contract_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/store/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Isolate each test in a new schema even when multiple processes use the same DB.
func postgresFixture(t *testing.T) func() *sql.DB {
	t.Helper()
	dsn := os.Getenv("TENDER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("set TENDER_TEST_DATABASE_URL to a disposable PostgreSQL database")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*cfg)
	schema := "tender_test_" + strings.ToLower(rand.Text())
	ident := pgx.Identifier{schema}.Sanitize()
	_, err = admin.ExecContext(t.Context(), "CREATE SCHEMA "+ident)
	admin.Close()
	if err != nil {
		t.Fatal(err)
	}
	adminConfig := cfg.Copy()
	t.Cleanup(func() {
		admin := stdlib.OpenDB(*adminConfig)
		defer admin.Close()
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+ident+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	open := func() *sql.DB {
		db := stdlib.OpenDB(*cfg, stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, "SET search_path TO "+ident)
			return err
		}))
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { db.Close() })
		return db
	}
	db := open()
	paths, err := fs.Glob(postgres.Migrations, "migrations/*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		migration, err := postgres.Migrations.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), string(migration)); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	db.Close()
	return open
}

var dbMarket = tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}
var dbScope = tender.Scope{MerchantID: "shop", Environment: tender.EnvironmentMock}
var dbPurchase = tender.Request{PaymentID: "payment", OrderID: "order", Key: "create", Amount: tender.Money{Minor: 100, Currency: "USD"}, Token: "opaque"}

type storageProvider struct {
	execute func(tender.ProviderRequest) (tender.Observation, error)
	query   func(tender.ProviderRequest) (tender.Observation, error)
}

func (p storageProvider) Capabilities() tender.Capabilities {
	return tender.Capabilities{Create: true, Capture: true, Refund: true, Query: true, Markets: []tender.MarketCapability{{Market: dbMarket, PartialCapture: true, PartialRefund: true}}}
}
func (p storageProvider) Execute(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
	return p.execute(r)
}
func (p storageProvider) Query(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
	return p.query(r)
}
func storageObservation(r tender.ProviderRequest, status tender.PaymentStatus, funds tender.Funds) tender.Observation {
	return tender.Observation{Scope: r.Payment.Scope, PaymentID: r.Payment.ID, AttemptID: r.Operation.AttemptID,
		MerchantAccount: r.Payment.MerchantAccount, OperationID: r.Operation.ID, ProviderReference: "charge", OperationReference: "effect-" + r.Operation.ID,
		Amount: r.Operation.Amount, Status: tender.OperationSucceeded, Snapshot: tender.Snapshot{Status: status, Funds: funds, VerifiedMethod: tender.MethodVisa}}
}
func storageEngine(t *testing.T, db *sql.DB, p storageProvider, now func() time.Time) *tender.Engine {
	t.Helper()
	e, err := tender.NewEngine(tender.EngineConfig{Store: postgres.New(db), Scope: dbScope, Provider: p, ProviderName: "processor", MerchantAccount: "account", Market: dbMarket, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestPostgresRestartRecovery(t *testing.T) {
	open := postgresFixture(t)
	ctx := t.Context()
	first := open()
	// A real database failure at the final outbox write must undo the result,
	// including the change in principal and the audit entry that preceded it.
	if _, err := first.ExecContext(ctx, `ALTER TABLE tender_outbox ADD CONSTRAINT fail_result CHECK (false)`); err != nil {
		t.Fatal(err)
	}
	var dispatched tender.Operation
	var calls atomic.Int32
	p := storageProvider{execute: func(r tender.ProviderRequest) (tender.Observation, error) {
		calls.Add(1)
		dispatched = r.Operation
		return storageObservation(r, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100}), nil
	}, query: func(r tender.ProviderRequest) (tender.Observation, error) {
		if r.Operation.ID != dispatched.ID || r.Operation.AttemptID != dispatched.AttemptID || r.Operation.ProviderKey != dispatched.ProviderKey || r.Operation.Fingerprint != dispatched.Fingerprint || r.Token != "" {
			t.Fatalf("recovery identity mismatch: %+v", r.Operation)
		}
		return storageObservation(r, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100}), nil
	}}
	r, err := storageEngine(t, first, p, nil).Create(ctx, dbPurchase)
	if !errors.Is(err, tender.ErrUnknownOutcome) || r.Operation.Status != tender.OperationUnknown || dispatched.ProviderKey == "" {
		t.Fatalf("failed result write = %+v, %v", r, err)
	}
	const inboxInsert = `INSERT INTO tender_provider_events(merchant_id,environment,provider,merchant_account,event_id,evidence) VALUES ('shop','MOCK','processor','account','event-1','{"reference":"charge"}') ON CONFLICT DO NOTHING`
	if _, err := first.ExecContext(ctx, inboxInsert); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := open()
	e := storageEngine(t, second, p, nil)
	replayed, err := e.Create(ctx, dbPurchase)
	if !errors.Is(err, tender.ErrUnknownOutcome) || replayed.Operation != r.Operation || replayed.Payment.Version != 1 || replayed.Payment.Funds.Captured != 0 {
		t.Fatalf("restart replay = %+v, %v", replayed, err)
	}
	s := postgres.New(second)
	history, err := s.History(ctx, dbScope, dbPurchase.PaymentID)
	if err != nil || len(history) != 2 || history[0].Source != "reservation" || history[1].Source != "dispatch_error" {
		t.Fatalf("failed result audit = %+v, %v", history, err)
	}
	out, err := s.Outbox(ctx, dbScope)
	if err != nil || len(out) != 0 {
		t.Fatalf("uncommitted result outbox = %+v, %v", out, err)
	}
	if _, err := second.ExecContext(ctx, `ALTER TABLE tender_outbox DROP CONSTRAINT fail_result`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		recovered, err := e.Recover(ctx, r.Operation.ID)
		if err != nil || recovered.Payment.Version != 2 || recovered.Payment.Funds.Captured != 100 {
			t.Fatalf("recover = %+v, %v", recovered, err)
		}
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	third := open()
	s = postgres.New(third)
	history, err = s.History(ctx, dbScope, dbPurchase.PaymentID)
	if err != nil || len(history) != 3 || history[2].Source != "query" || history[2].Change.Version != 2 {
		t.Fatalf("recovered history = %+v, %v", history, err)
	}
	out, err = s.Outbox(ctx, dbScope)
	if err != nil || len(out) != 1 || out[0].OperationID != dispatched.ID || out[0].Version != 2 || calls.Load() != 1 {
		t.Fatalf("outbox = %+v, %v; dispatches = %d", out, err, calls.Load())
	}
	duplicate, err := third.ExecContext(ctx, inboxInsert)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := duplicate.RowsAffected(); err != nil || n != 0 {
		t.Fatalf("inbox replay inserted %d rows: %v", n, err)
	}
	var evidence string
	if err := third.QueryRowContext(ctx, `SELECT evidence->>'reference' FROM tender_provider_events WHERE event_id = 'event-1' AND processed_at IS NULL`).Scan(&evidence); err != nil || evidence != "charge" {
		t.Fatalf("pending verified event = %q, %v", evidence, err)
	}
}

func TestPostgresConflictingOperations(t *testing.T) {
	open := postgresFixture(t)
	ctx := t.Context()
	var dispatches atomic.Int32
	p := storageProvider{execute: func(r tender.ProviderRequest) (tender.Observation, error) {
		dispatches.Add(1)
		f := r.Payment.Funds
		status := tender.PaymentAuthorized
		switch r.Operation.Kind {
		case tender.OpCreate:
			f.Authorized = 100
		case tender.OpCapture:
			f.Captured += r.Operation.Amount.Minor
			status = tender.PaymentPartiallyCaptured
		case tender.OpRefund:
			f.Refunded += r.Operation.Amount.Minor
			status = tender.PaymentPartiallyCaptured
		}
		return storageObservation(r, status, f), nil
	}}
	e1, e2 := storageEngine(t, open(), p, nil), storageEngine(t, open(), p, nil)
	race := func(a, b func() error) {
		t.Helper()
		start := make(chan struct{})
		outcomes := make(chan error, 2)
		for _, run := range []func() error{a, b} {
			go func() { <-start; outcomes <- run() }()
		}
		close(start)
		successes := 0
		for range 2 {
			err := <-outcomes
			if err == nil {
				successes++
			} else if !errors.Is(err, tender.ErrConflict) && !errors.Is(err, tender.ErrValidation) {
				t.Fatalf("competing mutation: %v", err)
			}
		}
		if successes != 1 {
			t.Fatalf("%d competing operations succeeded", successes)
		}
	}
	secondClaim := dbPurchase
	secondClaim.PaymentID = "other"
	secondClaim.Key = "other-key"
	race(func() error { _, err := e1.Create(ctx, dbPurchase); return err }, func() error { _, err := e2.Create(ctx, secondClaim); return err })
	var claim tender.Payment
	s := postgres.New(open())
	if err := s.Transact(ctx, func(tx tender.Tx) error { var err error; claim, err = tx.PaymentByOrder(dbScope, "order"); return err }); err != nil {
		t.Fatal(err)
	}
	mutate := func(e *tender.Engine, op tender.OperationKind, key string, n int64) func() error {
		return func() error {
			r := tender.Request{PaymentID: claim.ID, Key: key, Amount: tender.Money{Minor: n, Currency: "USD"}}
			if op == tender.OpCapture {
				_, err := e.Capture(ctx, r)
				return err
			}
			_, err := e.Refund(ctx, r)
			return err
		}
	}
	race(mutate(e1, tender.OpCapture, "capture-A", 75), mutate(e2, tender.OpCapture, "capture-B", 75))
	race(mutate(e1, tender.OpRefund, "refund-A", 50), mutate(e2, tender.OpRefund, "refund-B", 50))
	got, err := e1.Get(ctx, claim.ID)
	if err != nil || got.Version != 4 || got.Funds != (tender.Funds{Authorized: 100, Captured: 75, Refunded: 50}) || dispatches.Load() != 3 {
		t.Fatalf("principal = %+v, dispatches %d, error %v", got, dispatches.Load(), err)
	}
}

func TestPostgresRetryDeadlineAndSuccessor(t *testing.T) {
	open := postgresFixture(t)
	ctx := t.Context()
	now := time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)
	clock := func() time.Time { return now }
	p := storageProvider{execute: func(r tender.ProviderRequest) (tender.Observation, error) {
		if r.Operation.Key == "create" {
			return tender.Observation{}, &tender.ProviderError{Kind: tender.ErrRateLimit, RetryAfter: time.Minute}
		}
		return storageObservation(r, tender.PaymentAuthorized, tender.Funds{Authorized: 100}), nil
	}}
	first := open()
	r, err := storageEngine(t, first, p, clock).Create(ctx, dbPurchase)
	if !errors.Is(err, tender.ErrRateLimit) {
		t.Fatalf("rejection: %v", err)
	}
	first.Close()
	second := open()
	e := storageEngine(t, second, p, clock)
	newReq := dbPurchase
	newReq.Key = "successor"
	if _, err = e.RetryCreate(ctx, r.Operation.ID, newReq); !errors.Is(err, tender.ErrRateLimit) {
		t.Fatalf("early retry: %v", err)
	}
	now = now.Add(time.Minute)
	next, err := e.RetryCreate(ctx, r.Operation.ID, newReq)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
	third := open()
	err = postgres.New(third).Transact(ctx, func(tx tender.Tx) error {
		op, err := tx.OperationByKey(dbScope, "create")
		if err != nil {
			return err
		}
		if op.SuccessorID != next.Operation.ID || !op.RetryNotBefore.Equal(now) || op.RetryAfter != time.Minute {
			return fmt.Errorf("lost predecessor: %+v", op)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPostgresBindingAndVersionFences(t *testing.T) {
	open := postgresFixture(t)
	ctx := t.Context()
	s := postgres.New(open())
	p := tender.Payment{Scope: dbScope, ID: "p", OrderID: "o", AttemptID: "a", Provider: "processor", MerchantAccount: "acct", ProviderReference: "remote", Market: dbMarket, Amount: dbPurchase.Amount, Version: 1}
	o := tender.OperationRecord{Operation: tender.Operation{Scope: p.Scope, ID: "op", PaymentID: p.ID, AttemptID: p.AttemptID, Key: "key", Fingerprint: "hash", ProviderKey: "stable", ProviderReference: "remote-op", Kind: tender.OpCreate, Status: tender.OperationSucceeded, Amount: p.Amount, BaseVersion: 1}, PaymentVersion: 1}
	if err := s.Transact(ctx, func(tx tender.Tx) error {
		if err := tx.InsertPayment(p); err != nil {
			return err
		}
		return tx.InsertOperation(o)
	}); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*tender.Payment){func(p *tender.Payment) { p.MerchantAccount = "rerouted" }, func(p *tender.Payment) { p.ProviderReference = "rebound" }, func(p *tender.Payment) { p.Version++ }} {
		updated := p
		updated.Version++
		edit(&updated)
		if err := s.Transact(ctx, func(tx tender.Tx) error { return tx.UpdatePayment(updated) }); !errors.Is(err, tender.ErrConflict) {
			t.Fatalf("replaced payment binding or skipped a version: %v", err)
		}
	}
	for _, edit := range []func(*tender.OperationRecord){func(o *tender.OperationRecord) { o.ProviderKey = "new" }, func(o *tender.OperationRecord) { o.ProviderReference = "new" }} {
		updated := o
		edit(&updated)
		if err := s.Transact(ctx, func(tx tender.Tx) error { return tx.UpdateOperation(updated) }); !errors.Is(err, tender.ErrConflict) {
			t.Fatalf("replaced operation binding: %v", err)
		}
	}
	other := p
	other.ID = "p2"
	other.OrderID = "o2"
	other.AttemptID = "a2"
	if err := s.Transact(ctx, func(tx tender.Tx) error { return tx.InsertPayment(other) }); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("duplicate provider reference: %v", err)
	}
	other.MerchantAccount = "acct2"
	if err := s.Transact(ctx, func(tx tender.Tx) error { return tx.InsertPayment(other) }); err != nil {
		t.Fatalf("independent account reference: %v", err)
	}
	otherOp := o
	otherOp.ID = "op2"
	otherOp.Key = "other-key"
	otherOp.ProviderKey = "other-provider-key"
	if err := s.Transact(ctx, func(tx tender.Tx) error { return tx.InsertOperation(otherOp) }); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("duplicate operation reference: %v", err)
	}
	otherOp.PaymentID = other.ID
	otherOp.AttemptID = other.AttemptID
	if err := s.Transact(ctx, func(tx tender.Tx) error { return tx.InsertOperation(otherOp) }); err != nil {
		t.Fatalf("independent payment reference: %v", err)
	}
	if err := s.Transact(ctx, func(tx tender.Tx) error {
		a, err := tx.OperationByReference(dbScope, other.ID, "remote-op")
		if err != nil {
			return err
		}
		if a.ID != "op2" {
			return fmt.Errorf("bound wrong operation %q", a.ID)
		}
		_, err = tx.OperationByReference(tender.Scope{MerchantID: "other", Environment: tender.EnvironmentMock}, other.ID, "remote-op")
		return err
	}); !errors.Is(err, tender.ErrNotFound) {
		t.Fatalf("scoped lookup: %v", err)
	}
}

func TestPostgresRestartRecoveryBeforeResult(t *testing.T) {
	open := postgresFixture(t)
	ctx := t.Context()
	first := open()
	var reservation tender.Operation
	var mutations int
	p := storageProvider{
		execute: func(r tender.ProviderRequest) (tender.Observation, error) {
			mutations++
			reservation = r.Operation
			// Lose the application's pool as soon as the reservation has left
			// the process. Neither the result nor the uncertainty can be saved.
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			return tender.Observation{}, tender.ErrUnknownOutcome
		},
		query: func(r tender.ProviderRequest) (tender.Observation, error) {
			if r.Operation != reservation || r.Token != "" {
				t.Fatalf("lost dispatched reservation: %+v", r.Operation)
			}
			return storageObservation(r, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100}), nil
		},
	}
	initial, err := storageEngine(t, first, p, nil).Create(ctx, dbPurchase)
	if !errors.Is(err, tender.ErrUnknownOutcome) || initial.Operation.Status != tender.OperationReserved {
		t.Fatalf("lost result: %+v, %v", initial, err)
	}
	second := open()
	recoveredEngine := storageEngine(t, second, p, nil)
	replay, err := recoveredEngine.Create(ctx, dbPurchase)
	if err != nil || replay.Operation != reservation || mutations != 1 {
		t.Fatalf("replayed reservation = %+v, calls %d, %v", replay, mutations, err)
	}
	finished, err := recoveredEngine.Recover(ctx, reservation.ID)
	if err != nil || finished.Payment.Funds.Captured != 100 || finished.Payment.Version != 2 || mutations != 1 {
		t.Fatalf("recovered reservation = %+v, calls %d, %v", finished, mutations, err)
	}
}

func TestPostgresConflictingOperationsAbsentKeys(t *testing.T) {
	// Each worker may create its own order only if the other order is absent.
	// The keys are distinct, so unique indexes alone cannot prevent write skew.
	open := postgresFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	outcomes := make(chan error, 2)
	var callbacks atomic.Int32
	for _, orders := range [][2]string{{"one", "two"}, {"two", "one"}} {
		id, other := orders[0], orders[1]
		s := postgres.New(open())
		go func() {
			outcomes <- s.Transact(ctx, func(tx tender.Tx) error {
				callbacks.Add(1)
				if _, err := tx.PaymentByOrder(dbScope, other); !errors.Is(err, tender.ErrNotFound) {
					return fmt.Errorf("expected unused order %q, got %v", other, err)
				}
				ready <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				p, err := tender.NewPayment(dbScope, id, id, dbPurchase.Amount)
				if err != nil {
					return err
				}
				p.AttemptID = "attempt-" + id
				return tx.InsertPayment(p)
			})
		}()
	}
	for range 2 {
		select {
		case <-ready:
		case err := <-outcomes:
			t.Fatalf("worker stopped before barrier: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(release)
	first, second := <-outcomes, <-outcomes
	if callbacks.Load() != 2 || !(first == nil && errors.Is(second, tender.ErrConflict) || second == nil && errors.Is(first, tender.ErrConflict)) {
		t.Fatalf("conflicting absent reads returned %v and %v in %d callbacks", first, second, callbacks.Load())
	}
}

func TestPostgresLongOpaqueReferences(t *testing.T) {
	open := postgresFixture(t)
	ctx := t.Context()
	db := open()
	s := postgres.New(db)
	var bytes [3200]byte
	rand.Read(bytes[:])
	ref := base64.RawURLEncoding.EncodeToString(bytes[:])
	opRef := "op-" + ref
	p := storageProvider{execute: func(r tender.ProviderRequest) (tender.Observation, error) {
		o := storageObservation(r, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100})
		o.ProviderReference = ref
		o.OperationReference = opRef
		return o, nil
	}}
	got, err := storageEngine(t, db, p, nil).Create(ctx, dbPurchase)
	if err != nil {
		t.Fatalf("successful external charge could not commit: %v", err)
	}
	err = s.Transact(ctx, func(tx tender.Tx) error {
		o, err := tx.OperationByReference(dbScope, got.Payment.ID, opRef)
		if err != nil {
			return err
		}
		if o.ProviderReference != opRef || got.Payment.ProviderReference != ref {
			return fmt.Errorf("reference truncated")
		}
		_, err = tx.OperationByReference(dbScope, got.Payment.ID, opRef+"x")
		return err
	})
	if !errors.Is(err, tender.ErrNotFound) {
		t.Fatalf("exact lookup: %v", err)
	}
	duplicate := got.Payment
	duplicate.ID = "p2"
	duplicate.OrderID = "o2"
	duplicate.AttemptID = "a2"
	if err := s.Transact(ctx, func(tx tender.Tx) error { return tx.InsertPayment(duplicate) }); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("same route duplicate: %v", err)
	}
	duplicate.MerchantAccount = "other"
	if err := s.Transact(ctx, func(tx tender.Tx) error { return tx.InsertPayment(duplicate) }); err != nil {
		t.Fatalf("independent account reference: %v", err)
	}
}

func TestPostgresReconciliationRoute(t *testing.T) {
	open := postgresFixture(t)
	db := open()
	ctx := t.Context()
	s := postgres.New(db)
	for _, acct := range []string{"one", "two"} {
		p := tender.Payment{Scope: dbScope, ID: acct, OrderID: acct, AttemptID: acct, Provider: "processor", MerchantAccount: acct, ProviderReference: "same-reference", Amount: dbPurchase.Amount, Version: 1}
		if err := s.Transact(ctx, func(tx tender.Tx) error { return tx.InsertPayment(p) }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO tender_reconciliation_runs(merchant_id,environment,provider,merchant_account,run_id,report_id) VALUES ('shop','MOCK','processor','one','run','report')`); err != nil {
		t.Fatal(err)
	}
	const match = `INSERT INTO tender_reconciliation_history(merchant_id,environment,provider,merchant_account,run_id,line_id,payment_id,outcome,evidence) VALUES ('shop','MOCK','processor','one','run','line',$1,'matched','{}')`
	if _, err := db.ExecContext(ctx, match, "two"); err == nil {
		t.Fatal("report from account one matched a payment from account two")
	}
	if _, err := db.ExecContext(ctx, match, "one"); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresAtomicVerifiedReceipt(t *testing.T) {
	open := postgresFixture(t)
	ctx := t.Context()
	db := open()
	var capture tender.Observation
	p := storageProvider{execute: func(r tender.ProviderRequest) (tender.Observation, error) {
		if r.Operation.Kind == tender.OpRefund {
			return storageObservation(r, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100, Refunded: 30}), nil
		}
		capture = storageObservation(r, tender.PaymentCaptured, tender.Funds{Authorized: 100, Captured: 100})
		return tender.Observation{}, tender.ErrUnknownOutcome
	}}
	e := storageEngine(t, db, p, nil)
	if _, err := e.Create(ctx, dbPurchase); !errors.Is(err, tender.ErrUnknownOutcome) {
		t.Fatal(err)
	}
	wire := tender.VerifiedEvent{ID: "capture-event", Provider: "processor", Observation: capture}
	evidence, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	key := tender.VerifiedEventKey{Scope: dbScope, Provider: "processor", MerchantAccount: "account", ID: wire.ID}
	err = postgres.New(db).Transact(ctx, func(tx tender.Tx) error { return tx.(tender.VerifiedEventTx).InsertVerifiedEvent(key, evidence) })
	if err != nil {
		t.Fatal(err)
	}
	process := func() error {
		return e.ProcessVerifiedEvent(ctx, "capture-event", func(b json.RawMessage) (tender.VerifiedEvent, error) {
			var event tender.VerifiedEvent
			err := json.Unmarshal(b, &event)
			return event, err
		})
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE tender_outbox ADD CONSTRAINT reject_outbox CHECK (false)`); err != nil {
		t.Fatal(err)
	}
	if err := process(); err == nil {
		t.Fatal("outbox failure acknowledged verified capture")
	}
	var applied bool
	const check = `SELECT processed_at IS NOT NULL FROM tender_provider_events WHERE event_id='capture-event'`
	if err := db.QueryRowContext(ctx, check).Scan(&applied); err != nil || applied {
		t.Fatalf("failed event marked applied: %t %v", applied, err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE tender_outbox DROP CONSTRAINT reject_outbox`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE tender_provider_events ADD CONSTRAINT fail_receipt CHECK (processed_at IS NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := process(); err == nil {
		t.Fatal("receipt failure left capture applied")
	}
	before, err := e.Get(ctx, dbPurchase.PaymentID)
	if err != nil || before.Version != 1 || before.Funds.Captured != 0 {
		t.Fatalf("capture survived receipt failure: %+v, %v", before, err)
	}
	notices, err := postgres.New(db).Outbox(ctx, dbScope)
	if err != nil || len(notices) != 0 {
		t.Fatalf("outbox survived receipt failure: %+v %v", notices, err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE tender_provider_events DROP CONSTRAINT fail_receipt`); err != nil {
		t.Fatal(err)
	}
	if err := process(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db = open()
	e = storageEngine(t, db, p, nil)
	req := tender.Request{PaymentID: dbPurchase.PaymentID, Key: "refund", Amount: tender.Money{Minor: 30, Currency: "USD"}}
	if _, err := e.Refund(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := process(); err != nil {
		t.Fatalf("capture delivery replay after refund: %v", err)
	}
	if err := db.QueryRowContext(ctx, check).Scan(&applied); err != nil || !applied {
		t.Fatalf("successful event lacks receipt: %t %v", applied, err)
	}
	got, err := e.Get(ctx, dbPurchase.PaymentID)
	if err != nil || got.Funds.Refunded != 30 || got.Version != 3 {
		t.Fatalf("replay changed payment: %+v, %v", got, err)
	}
	out, err := postgres.New(db).Outbox(ctx, dbScope)
	if err != nil || len(out) != 1 {
		t.Fatalf("collection notices %v, %v", out, err)
	}
}
