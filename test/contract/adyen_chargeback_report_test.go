//go:build integration

package contract_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/providers/adyen"
	"github.com/gravestench/tender/store/postgres"
)

// A signed dispute and the same payment's captured modification must both be
// required when Adyen reports the capture PSP in a Chargeback journal row.
func TestReconcileAdyenChargebackCaptureReference(t *testing.T) {
	db := postgresFixture(t)()
	defer db.Close()
	ctx := t.Context()
	now := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
	scope := tender.Scope{MerchantID: "chargeback-report-shop", Environment: tender.EnvironmentMock}
	market := tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}
	store := postgres.New(db)
	p, err := tender.NewPayment(scope, "payment", "order", tender.Money{Minor: 100, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	p.Provider, p.MerchantAccount, p.Market, p.AttemptID, p.ProviderReference = "adyen", "acct", market, "attempt", "PAYMENTPSP"
	if _, err := p.Apply(tender.Snapshot{Status: tender.PaymentCaptured, VerifiedMethod: tender.MethodVisa, Funds: tender.Funds{Authorized: 100, Captured: 100}}, now); err != nil {
		t.Fatal(err)
	}
	op := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: "capture", PaymentID: p.ID, AttemptID: p.AttemptID, Key: "capture-key", ProviderReference: "CAPTUREPSP", Kind: tender.OpCapture, Status: tender.OperationSucceeded, Amount: p.Amount, BaseVersion: 1}}
	if err := store.Transact(ctx, func(tx tender.Tx) error {
		if err := tx.InsertPayment(p); err != nil {
			return err
		}
		return tx.InsertOperation(op)
	}); err != nil {
		t.Fatal(err)
	}
	engine, err := tender.NewEngine(tender.EngineConfig{Store: store, Provider: storageProvider{}, Scope: scope, ProviderName: "adyen", MerchantAccount: "acct", Market: market, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	row := map[string]string{
		"Merchant Account": "acct", "Tender Merchant Account": "acct", "Psp Reference": "PAYMENTPSP",
		"Merchant Reference": "chargeback-reference", "Payment Method": "visa", "Type": "Chargeback",
		"Modification Reference": "CAPTUREPSP", "Gross Currency": "USD", "Gross Debit (GC)": "1.00",
		"Net Currency": "USD", "Net Debit (NC)": "1.00", "Commission (NC)": "0.00", "Batch Number": "42",
		"Tender Source Part ID": "part-1", "Tender Source Section ID": "section-1",
	}
	line, err := adyen.ProjectCardSettlement(row, now)
	if err != nil {
		t.Fatal(err)
	}
	report := tender.SettlementReport{ID: "chargeback-capture-report", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{line}}
	before, err := engine.Reconcile(ctx, report)
	if err != nil || before.Results[0].Outcome != tender.ReconciliationUnconfirmed {
		t.Fatalf("report without signed dispute: %+v, %v", before, err)
	}
	notice := tender.AdverseNotice{Scope: scope, Provider: "adyen", MerchantAccount: "acct", PaymentID: p.ID,
		ProviderEventID: "dispute-event", Kind: "chargeback", ProviderReference: "DISPUTEPSP",
		OriginalReference: p.ProviderReference, ModificationReference: "DISPUTEPSP", Amount: p.Amount, ObservedAt: now}
	raw, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, func(tx tender.Tx) error {
		return tx.(tender.VerifiedEventTx).InsertVerifiedEvent(tender.VerifiedEventKey{Scope: scope, Provider: "adyen", MerchantAccount: "acct", ID: notice.ProviderEventID}, raw)
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.ProcessVerifiedAdverseEvent(ctx, notice.ProviderEventID, func(_ tender.EventRecords, raw json.RawMessage) (tender.AdverseNotice, error) {
		var n tender.AdverseNotice
		err := json.Unmarshal(raw, &n)
		return n, err
	}); err != nil {
		t.Fatal(err)
	}
	discrepant := report
	discrepant.ID = "incomplete-chargeback-report"
	discrepant.Lines = append([]tender.SettlementLine(nil), line)
	discrepant.Lines = append(discrepant.Lines, tender.SettlementLine{ID: "unknown-sale", ProviderReference: "UNKNOWN", Kind: tender.SettlementCaptured, Currency: "USD", GrossMinor: 100, NetMinor: 100, ObservedAt: now})
	incomplete, err := engine.Reconcile(ctx, discrepant)
	if err != nil || incomplete.Results[0].Outcome != tender.ReconciliationUnconfirmed {
		t.Fatalf("discrepant report spent signed dispute: %+v, %v", incomplete, err)
	}
	var claims int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM tender_reconciliation_return_claims WHERE merchant_id=$1 AND environment=$2 AND provider='adyen' AND merchant_account='acct' AND event_id=$3`, scope.MerchantID, scope.Environment, notice.ProviderEventID).Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("discrepant report persisted %d return claims: %v", claims, err)
	}
	after, err := engine.Reconcile(ctx, report)
	if err != nil || after.Results[0].Outcome != tender.ReconciliationMatched {
		t.Fatalf("signed dispute plus exact capture remained unconfirmed: %+v, %v", after, err)
	}
	replay, err := engine.Reconcile(ctx, report)
	if err != nil || replay.Results[0].Outcome != tender.ReconciliationMatched {
		t.Fatalf("same report replay lost its dispute claim: %+v, %v", replay, err)
	}
	otherReport := report
	otherReport.ID = "duplicated-chargeback-report"
	duplicate, err := engine.Reconcile(ctx, otherReport)
	if err != nil || duplicate.Results[0].Outcome != tender.ReconciliationUnconfirmed {
		t.Fatalf("one signed dispute matched across reports: %+v, %v", duplicate, err)
	}
	exact := report
	exact.ID = "duplicated-exact-dispute-report"
	exact.Lines = []tender.SettlementLine{line}
	exact.Lines[0].ModificationReference = notice.ModificationReference
	exactDuplicate, err := engine.Reconcile(ctx, exact)
	if err != nil || exactDuplicate.Results[0].Outcome != tender.ReconciliationUnconfirmed {
		t.Fatalf("capture and dispute PSP variants spent one notice twice: %+v, %v", exactDuplicate, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM tender_reconciliation_return_claims WHERE merchant_id=$1 AND environment=$2 AND provider='adyen' AND merchant_account='acct' AND event_id=$3`, scope.MerchantID, scope.Environment, notice.ProviderEventID).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("durable return claim count %d: %v", claims, err)
	}
}

// Both workers read the same SERIALIZABLE snapshot before either starts its
// report. A table lock acquired later must still leave only one report with a
// durable claim; a serialization loser retries from a fresh snapshot.
func TestConcurrentAdyenReturnClaims(t *testing.T) {
	open := postgresFixture(t)
	firstDB, secondDB := open(), open()
	defer firstDB.Close()
	defer secondDB.Close()
	var version string
	if err := firstDB.QueryRowContext(t.Context(), `SELECT version()`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(version, "emcc") {
		t.Skip("PGlite executes transactions in one emulated PostgreSQL process; this contract needs two native sessions")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	now := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
	scope := tender.Scope{MerchantID: "concurrent-return-shop", Environment: tender.EnvironmentMock}
	market := tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}
	base := postgres.New(firstDB)
	p, err := tender.NewPayment(scope, "payment", "order", tender.Money{Minor: 100, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	p.Provider, p.MerchantAccount, p.Market, p.AttemptID, p.ProviderReference = "adyen", "acct", market, "attempt", "PAYMENTPSP"
	if _, err := p.Apply(tender.Snapshot{Status: tender.PaymentCaptured, VerifiedMethod: tender.MethodVisa, Funds: tender.Funds{Authorized: 100, Captured: 100}}, now); err != nil {
		t.Fatal(err)
	}
	op := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: "capture", PaymentID: p.ID, AttemptID: p.AttemptID, Key: "capture-key", ProviderReference: "CAPTUREPSP", Kind: tender.OpCapture, Status: tender.OperationSucceeded, Amount: p.Amount, BaseVersion: 1}}
	if err := base.Transact(ctx, func(tx tender.Tx) error {
		if err := tx.InsertPayment(p); err != nil {
			return err
		}
		return tx.InsertOperation(op)
	}); err != nil {
		t.Fatal(err)
	}
	notice := tender.AdverseNotice{Scope: scope, Provider: "adyen", MerchantAccount: "acct", PaymentID: p.ID,
		ProviderEventID: "dispute-event", Kind: "chargeback", ProviderReference: "DISPUTEPSP",
		OriginalReference: p.ProviderReference, ModificationReference: "DISPUTEPSP", Amount: p.Amount, ObservedAt: now}
	raw, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	if err := base.Transact(ctx, func(tx tender.Tx) error {
		return tx.(tender.VerifiedEventTx).InsertVerifiedEvent(tender.VerifiedEventKey{Scope: scope, Provider: "adyen", MerchantAccount: "acct", ID: notice.ProviderEventID}, raw)
	}); err != nil {
		t.Fatal(err)
	}
	newEngine := func(s tender.Store) *tender.Engine {
		engine, err := tender.NewEngine(tender.EngineConfig{Store: s, Provider: storageProvider{}, Scope: scope, ProviderName: "adyen", MerchantAccount: "acct", Market: market, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		return engine
	}
	if err := newEngine(base).ProcessVerifiedAdverseEvent(ctx, notice.ProviderEventID, func(_ tender.EventRecords, raw json.RawMessage) (tender.AdverseNotice, error) {
		var n tender.AdverseNotice
		err := json.Unmarshal(raw, &n)
		return n, err
	}); err != nil {
		t.Fatal(err)
	}
	line, err := adyen.ProjectCardSettlement(map[string]string{
		"Merchant Account": "acct", "Tender Merchant Account": "acct", "Psp Reference": "PAYMENTPSP",
		"Merchant Reference": "concurrent-return", "Payment Method": "visa", "Type": "Chargeback",
		"Modification Reference": "CAPTUREPSP", "Gross Currency": "USD", "Gross Debit (GC)": "1.00",
		"Net Currency": "USD", "Net Debit (NC)": "1.00", "Commission (NC)": "0.00", "Batch Number": "42",
		"Tender Source Part ID": "part-1", "Tender Source Section ID": "section-1",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	first := newEngine(snapshotBarrierStore{inner: base, scope: scope, paymentID: p.ID, ready: ready, release: release})
	secondStore := postgres.New(secondDB)
	second := newEngine(snapshotBarrierStore{inner: secondStore, scope: scope, paymentID: p.ID, ready: ready, release: release})
	reports := []tender.SettlementReport{
		{ID: "concurrent-report-a", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{line}},
		{ID: "concurrent-report-b", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{line}},
	}
	type result struct {
		index int
		run   tender.ReconciliationRun
		err   error
	}
	out := make(chan result, 2)
	for i, engine := range []*tender.Engine{first, second} {
		go func(i int, engine *tender.Engine) {
			run, err := engine.Reconcile(ctx, reports[i])
			out <- result{i, run, err}
		}(i, engine)
	}
	for range 2 {
		select {
		case <-ready:
		case got := <-out:
			t.Fatalf("worker %d failed before snapshot barrier: %v", got.index, got.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(release)
	results := make([]result, 2)
	for range 2 {
		select {
		case got := <-out:
			results[got.index] = got
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	matched := 0
	for i, got := range results {
		if errors.Is(got.err, tender.ErrConflict) {
			got.run, got.err = newEngine([]tender.Store{base, secondStore}[i]).Reconcile(ctx, reports[i])
		}
		if got.err != nil {
			t.Fatalf("worker %d returned an unexpected error: %v", i, got.err)
		}
		if len(got.run.Results) != 1 {
			t.Fatalf("worker %d returned %d rows", i, len(got.run.Results))
		}
		switch got.run.Results[0].Outcome {
		case tender.ReconciliationMatched:
			matched++
		case tender.ReconciliationUnconfirmed:
		default:
			t.Fatalf("worker %d returned %s", i, got.run.Results[0].Outcome)
		}
	}
	var claims int
	if err := firstDB.QueryRowContext(ctx, `SELECT count(*) FROM tender_reconciliation_return_claims WHERE merchant_id=$1 AND environment=$2 AND provider='adyen' AND merchant_account='acct' AND event_id=$3`, scope.MerchantID, scope.Environment, notice.ProviderEventID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if matched != 1 || claims != 1 {
		t.Fatalf("competing reports: %d matched, %d durable claims", matched, claims)
	}
}

type snapshotBarrierStore struct {
	inner     tender.Store
	scope     tender.Scope
	paymentID string
	ready     chan<- struct{}
	release   <-chan struct{}
}

func (s snapshotBarrierStore) Transact(ctx context.Context, fn func(tender.Tx) error) error {
	return s.inner.Transact(ctx, func(tx tender.Tx) error {
		if _, err := tx.Payment(s.scope, s.paymentID); err != nil {
			return err
		}
		s.ready <- struct{}{}
		select {
		case <-s.release:
			return fn(tx)
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

func TestReturnClaimMigrationRoundTrip(t *testing.T) {
	db := postgresFixture(t)()
	defer db.Close()
	for _, path := range []string{"migrations/007_return_evidence_claims.down.sql", "migrations/007_return_evidence_claims.up.sql"} {
		migration, err := postgres.Migrations.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), string(migration)); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	var table string
	if err := db.QueryRowContext(t.Context(), `SELECT to_regclass('tender_reconciliation_return_claims')::text`).Scan(&table); err != nil || table != "tender_reconciliation_return_claims" {
		t.Fatalf("return claim migration round trip left table %q: %v", table, err)
	}
}
