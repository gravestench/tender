//go:build integration

package contract_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/store/postgres"
)

func bankDepositFixture(t *testing.T) (func() *sql.DB, *sql.DB, *tender.Engine, tender.Scope, tender.BankDepositEvidence) {
	t.Helper()
	open := postgresFixture(t)
	db := open()
	scope := tender.Scope{MerchantID: "deposit-shop", Environment: tender.EnvironmentMock}
	market := tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}
	store := postgres.New(db)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	p, err := tender.NewPayment(scope, "sale", "order-sale", tender.Money{Minor: 10000, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	p.Provider, p.MerchantAccount, p.Market, p.AttemptID, p.ProviderReference = "adyen", "acct", market, "attempt-sale", "psp-sale"
	if _, err := p.Apply(tender.Snapshot{Status: tender.PaymentCaptured, Funds: tender.Funds{Authorized: 10000, Captured: 10000}}, now); err != nil {
		t.Fatal(err)
	}
	op := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: "op-sale", PaymentID: p.ID, AttemptID: p.AttemptID, Key: "key-sale", ProviderReference: "mod-sale", Kind: tender.OpCapture, Status: tender.OperationSucceeded, Amount: tender.Money{Minor: 10000, Currency: "USD"}, BaseVersion: 1}}
	if err := store.Transact(t.Context(), func(tx tender.Tx) error {
		if err := tx.InsertPayment(p); err != nil {
			return err
		}
		return tx.InsertOperation(op)
	}); err != nil {
		t.Fatal(err)
	}
	engine, err := tender.NewEngine(tender.EngineConfig{Store: store, Provider: storageProvider{}, Scope: scope, ProviderName: "adyen", MerchantAccount: "acct", Market: market})
	if err != nil {
		t.Fatal(err)
	}
	net := int64(9700)
	report := tender.SettlementReport{ID: "report-1", Feed: tender.FeedHostVerified,
		Lines:    []tender.SettlementLine{{ID: "sale-row", ProviderReference: "mod-sale", OriginalReference: "psp-sale", ModificationReference: "mod-sale", BatchID: "batch-1", SourcePartID: "part-1", SourceSectionID: "section-1", Kind: tender.SettlementCaptured, Currency: "USD", GrossMinor: 10000, FeeMinor: 300, NetMinor: net, ObservedAt: now}},
		Payouts:  []tender.PayoutLine{{ID: "payout-1", BatchID: "batch-1", SourcePartID: "part-1", SourceSectionID: "section-1", Currency: "USD", PayoutReference: "TX123XT", AmountMinor: net, ObservedAt: now}},
		Parts:    []tender.ReportPart{{ID: "part-1", SectionID: "section-1", Digest: "authenticated-provider-artifact", Number: 1, TotalParts: 1, DeclaredRows: 2}},
		Sections: []tender.ReportSection{{ID: "section-1", BatchID: "batch-1", Currency: "USD", Number: 1, TotalSections: 1, DeclaredRows: 2, DeclaredNetMinor: &net, DeclaredPayoutMinor: &net}},
	}
	run, err := engine.Reconcile(t.Context(), report)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.PayoutResults) != 1 || run.PayoutResults[0].Outcome != tender.PayoutMatched {
		t.Fatalf("provider payout prerequisite: %+v", run.PayoutResults)
	}
	evidence := tender.BankDepositEvidence{Scope: scope, BankAccountToken: "opaque-bank-token", MovementID: "movement-1", StatementSHA256: strings.Repeat("a", 64), PayoutReference: "TX123XT", Currency: "USD", AmountMinor: net, BookedAt: now.Add(24 * time.Hour)}
	return open, db, engine, scope, evidence
}

func TestBankDepositEvidenceDurability(t *testing.T) {
	open, db, engine, scope, evidence := bankDepositFixture(t)
	wrong := evidence
	wrong.MovementID = "movement-2"
	wrong.AmountMinor--
	result, err := engine.ReconcileBankDeposit(t.Context(), "report-1", "payout-1", wrong)
	if err != nil || result.Outcome != tender.BankDepositAmountMismatch {
		t.Fatalf("amount mismatch: %+v, %v", result, err)
	}
	wrongReference := evidence
	wrongReference.MovementID = "movement-ref-mismatch"
	wrongReference.PayoutReference = "TXWRONGXT"
	if result, err := engine.ReconcileBankDeposit(t.Context(), "report-1", "payout-1", wrongReference); err != nil || result.Outcome != tender.BankDepositReferenceMismatch {
		t.Fatalf("reference mismatch: %+v, %v", result, err)
	}
	wrongCurrency := evidence
	wrongCurrency.MovementID = "movement-currency-mismatch"
	wrongCurrency.Currency = "EUR"
	if result, err := engine.ReconcileBankDeposit(t.Context(), "report-1", "payout-1", wrongCurrency); err != nil || result.Outcome != tender.BankDepositCurrencyMismatch {
		t.Fatalf("currency mismatch: %+v, %v", result, err)
	}
	got, err := engine.ReconcileBankDeposit(t.Context(), "report-1", "payout-1", evidence)
	if err != nil || got.Outcome != tender.BankDepositMatched {
		t.Fatalf("exact deposit: %+v, %v", got, err)
	}
	again, err := engine.ReconcileBankDeposit(t.Context(), "report-1", "payout-1", evidence)
	if err != nil || again != got {
		t.Fatalf("replay: %+v, %v", again, err)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM tender_bank_deposit_reviews WHERE merchant_id=$1`, scope.MerchantID).Scan(&count); err != nil || count != 4 {
		t.Fatalf("unexpected %d reviews: %v", count, err)
	}
	changed := evidence
	changed.StatementSHA256 = strings.Repeat("b", 64)
	if _, err := engine.ReconcileBankDeposit(t.Context(), "report-1", "payout-1", changed); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("changed statement identity: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = open()
	restarted, err := tender.NewEngine(tender.EngineConfig{Store: postgres.New(db), Provider: storageProvider{}, Scope: scope, ProviderName: "adyen", MerchantAccount: "acct", Market: tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}})
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := restarted.ReconcileBankDeposit(t.Context(), "report-1", "payout-1", evidence); err != nil || replay.Outcome != tender.BankDepositMatched {
		t.Fatalf("restart replay: %+v, %v", replay, err)
	}
}

func TestBankDepositEvidenceValidation(t *testing.T) {
	_, _, engine, _, evidence := bankDepositFixture(t)
	for _, mutate := range []func(*tender.BankDepositEvidence){
		func(e *tender.BankDepositEvidence) { e.Scope.MerchantID = "other" },
		func(e *tender.BankDepositEvidence) { e.StatementSHA256 = "not-a-digest" },
		func(e *tender.BankDepositEvidence) { e.AmountMinor = 0 },
		func(e *tender.BankDepositEvidence) { e.BookedAt = time.Time{} },
		func(e *tender.BankDepositEvidence) { e.PayoutReference = "TX123XT bank account 123456" },
		func(e *tender.BankDepositEvidence) { e.MovementID = "statement line with text" },
		func(e *tender.BankDepositEvidence) { e.BankAccountToken = "123456789" },
	} {
		e := evidence
		mutate(&e)
		if _, err := engine.ReconcileBankDeposit(context.Background(), "report-1", "payout-1", e); !errors.Is(err, tender.ErrValidation) {
			t.Fatalf("accepted invalid evidence %+v: %v", e, err)
		}
	}
}

func seedAdditionalBankPayout(t *testing.T, db *sql.DB, engine *tender.Engine, scope tender.Scope, suffix, reference string) {
	t.Helper()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	p, err := tender.NewPayment(scope, "sale-"+suffix, "order-"+suffix, tender.Money{Minor: 5000, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	p.Provider, p.MerchantAccount, p.Market, p.AttemptID, p.ProviderReference = "adyen", "acct", tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}, "attempt-"+suffix, "psp-"+suffix
	if _, err := p.Apply(tender.Snapshot{Status: tender.PaymentCaptured, Funds: tender.Funds{Authorized: 5000, Captured: 5000}}, now); err != nil {
		t.Fatal(err)
	}
	op := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: "op-" + suffix, PaymentID: p.ID, AttemptID: p.AttemptID, Key: "key-" + suffix, ProviderReference: "mod-" + suffix, Kind: tender.OpCapture, Status: tender.OperationSucceeded, Amount: tender.Money{Minor: 5000, Currency: "USD"}, BaseVersion: 1}}
	if err := postgres.New(db).Transact(t.Context(), func(tx tender.Tx) error {
		if err := tx.InsertPayment(p); err != nil {
			return err
		}
		return tx.InsertOperation(op)
	}); err != nil {
		t.Fatal(err)
	}
	total := int64(5000)
	report := tender.SettlementReport{ID: "report-" + suffix, Feed: tender.FeedHostVerified,
		Lines:    []tender.SettlementLine{{ID: "sale-row-" + suffix, ProviderReference: op.ProviderReference, OriginalReference: p.ProviderReference, ModificationReference: op.ProviderReference, BatchID: "batch-" + suffix, SourcePartID: "part-" + suffix, SourceSectionID: "section-" + suffix, Kind: tender.SettlementCaptured, Currency: "USD", GrossMinor: total, NetMinor: total, ObservedAt: now}},
		Payouts:  []tender.PayoutLine{{ID: "payout-" + suffix, BatchID: "batch-" + suffix, SourcePartID: "part-" + suffix, SourceSectionID: "section-" + suffix, Currency: "USD", PayoutReference: reference, AmountMinor: total, ObservedAt: now}},
		Parts:    []tender.ReportPart{{ID: "part-" + suffix, SectionID: "section-" + suffix, Digest: "authenticated-provider-artifact-" + suffix, Number: 1, TotalParts: 1, DeclaredRows: 2}},
		Sections: []tender.ReportSection{{ID: "section-" + suffix, BatchID: "batch-" + suffix, Currency: "USD", Number: 1, TotalSections: 1, DeclaredRows: 2, DeclaredNetMinor: &total, DeclaredPayoutMinor: &total}},
	}
	run, err := engine.Reconcile(t.Context(), report)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.PayoutResults) != 1 || run.PayoutResults[0].Outcome != tender.PayoutMatched {
		t.Fatalf("additional provider payout: %+v", run.PayoutResults)
	}
}

func TestBankDepositReusedMovementAndProviderGate(t *testing.T) {
	_, db, engine, scope, first := bankDepositFixture(t)
	if got, err := engine.ReconcileBankDeposit(t.Context(), "report-1", "payout-1", first); err != nil || got.Outcome != tender.BankDepositMatched {
		t.Fatalf("first deposit: %+v, %v", got, err)
	}
	seedAdditionalBankPayout(t, db, engine, scope, "2", "TX456XT")
	second := first
	second.AmountMinor = 5000
	second.PayoutReference = "TX456XT"
	if got, err := engine.ReconcileBankDeposit(t.Context(), "report-2", "payout-2", second); err != nil || got.Outcome != tender.BankDepositReused {
		t.Fatalf("reused bank movement: %+v, %v", got, err)
	}
	second.MovementID = "movement-2"
	second.PayoutReference = "TX123XT"
	if got, err := engine.ReconcileBankDeposit(t.Context(), "report-2", "payout-2", second); err != nil || got.Outcome != tender.BankDepositReused {
		t.Fatalf("reused payout reference: %+v, %v", got, err)
	}
	second.MovementID = "movement-3"
	second.PayoutReference = "TX456XT"
	if got, err := engine.ReconcileBankDeposit(t.Context(), "report-2", "payout-2", second); err != nil || got.Outcome != tender.BankDepositMatched {
		t.Fatalf("unclaimed second movement: %+v, %v", got, err)
	}
	if _, err := engine.ReconcileBankDeposit(t.Context(), "missing-report", "payout", second); !errors.Is(err, tender.ErrNotFound) {
		t.Fatalf("missing provider report: %v", err)
	}
	incomplete := tender.SettlementReport{ID: "incomplete-report", Feed: tender.FeedHostVerified, Payouts: []tender.PayoutLine{{ID: "incomplete-payout", Currency: "USD", PayoutReference: "TX999XT", AmountMinor: 100, ObservedAt: time.Now()}}}
	run, err := engine.Reconcile(t.Context(), incomplete)
	if err != nil || len(run.PayoutResults) != 1 || run.PayoutResults[0].Outcome != tender.PayoutIncomplete {
		t.Fatalf("provider incomplete: %+v, %v", run.PayoutResults, err)
	}
	second.MovementID = "movement-4"
	second.PayoutReference = "TX999XT"
	second.AmountMinor = 100
	if got, err := engine.ReconcileBankDeposit(t.Context(), "incomplete-report", "incomplete-payout", second); err != nil || got.Outcome != tender.BankDepositProviderUnconfirmed {
		t.Fatalf("unconfirmed provider payout: %+v, %v", got, err)
	}
}

// Native PostgreSQL's serializable transaction and unique claim indexes decide
// this race. PGlite socket is single-connection and cannot prove that property.
func TestBankDepositConcurrentClaims(t *testing.T) {
	if os.Getenv("TENDER_TEST_PGLITE") != "" {
		t.Skip("PGlite socket cannot prove multi-backend serialization")
	}
	_, db, engine, scope, first := bankDepositFixture(t)
	seedAdditionalBankPayout(t, db, engine, scope, "2", "TX456XT")
	db.SetMaxOpenConns(4)
	type attempt struct {
		report, payout string
		evidence       tender.BankDepositEvidence
	}
	second := first
	second.PayoutReference = "TX456XT"
	second.AmountMinor = 5000
	attempts := []attempt{{"report-1", "payout-1", first}, {"report-2", "payout-2", second}}
	var wg sync.WaitGroup
	type response struct {
		index  int
		result tender.BankDepositResult
		err    error
	}
	replies := make(chan response, 2)
	for i, a := range attempts {
		wg.Add(1)
		go func(i int, a attempt) {
			defer wg.Done()
			result, err := engine.ReconcileBankDeposit(t.Context(), a.report, a.payout, a.evidence)
			replies <- response{i, result, err}
		}(i, a)
	}
	wg.Wait()
	close(replies)
	matched := 0
	for reply := range replies {
		if errors.Is(reply.err, tender.ErrConflict) {
			a := attempts[reply.index]
			reply.result, reply.err = engine.ReconcileBankDeposit(t.Context(), a.report, a.payout, a.evidence)
		}
		if reply.err != nil {
			t.Fatal(reply.err)
		}
		switch reply.result.Outcome {
		case tender.BankDepositMatched:
			matched++
		case tender.BankDepositReused:
		default:
			t.Fatalf("concurrent result: %+v", reply.result)
		}
	}
	if matched != 1 {
		t.Fatalf("%d movements matched; want exactly one", matched)
	}
}

func TestBankDepositFollowsLatestProviderVerdict(t *testing.T) {
	_, db, engine, scope, evidence := bankDepositFixture(t)
	if got, err := engine.ReconcileBankDeposit(t.Context(), "report-1", "payout-1", evidence); err != nil || got.Outcome != tender.BankDepositMatched {
		t.Fatalf("initial match: %+v, %v", got, err)
	}
	var firstRecordedAt time.Time
	if err := db.QueryRowContext(t.Context(), `SELECT recorded_at FROM tender_bank_deposit_reviews WHERE report_id='report-1' AND payout_id='payout-1'`).Scan(&firstRecordedAt); err != nil {
		t.Fatal(err)
	}
	store := postgres.New(db)
	var initial tender.PayoutReconciliationResult
	if err := store.Transact(t.Context(), func(tx tender.Tx) error {
		var err error
		initial, err = tx.(tender.BankDepositTx).PayoutResult(scope, "adyen", "acct", "report-1", "payout-1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	demoted := initial
	demoted.Outcome = tender.PayoutUnconfirmed
	if err := store.Transact(t.Context(), func(tx tender.Tx) error {
		return tx.(tender.ReconciliationTx).AppendPayoutReconciliationResult(scope, "adyen", "acct", "report-1", demoted)
	}); err != nil {
		t.Fatal(err)
	}
	// No bank replay occurs here. The persisted operational view must already be safe.
	var currentOutcome, currentDigest string
	var currentAt time.Time
	if err := db.QueryRowContext(t.Context(), `SELECT outcome,evidence->'Evidence'->>'StatementSHA256',recorded_at FROM tender_bank_deposit_reviews WHERE report_id='report-1' AND payout_id='payout-1'`).Scan(&currentOutcome, &currentDigest, &currentAt); err != nil {
		t.Fatal(err)
	}
	if currentOutcome != string(tender.BankDepositProviderUnconfirmed) || currentDigest != evidence.StatementSHA256 {
		t.Fatalf("stale or changed current bank review: %s %s", currentOutcome, currentDigest)
	}
	checkHistory := func(want ...tender.BankDepositOutcome) {
		t.Helper()
		rows, err := db.QueryContext(t.Context(), `SELECT outcome,evidence->'Evidence'->>'StatementSHA256',recorded_at FROM tender_bank_deposit_review_history WHERE report_id='report-1' AND payout_id='payout-1' ORDER BY seq`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		i := 0
		for rows.Next() {
			var outcome, digest string
			var at time.Time
			if err := rows.Scan(&outcome, &digest, &at); err != nil {
				t.Fatal(err)
			}
			if i >= len(want) || outcome != string(want[i]) || digest != evidence.StatementSHA256 {
				t.Fatalf("history row %d: %s %s", i, outcome, digest)
			}
			if i == 0 && !at.Equal(firstRecordedAt) {
				t.Fatalf("original verdict timestamp changed: %s != %s", at, firstRecordedAt)
			}
			if i == len(want)-1 && !at.Equal(currentAt) {
				t.Fatalf("current timestamp %s differs from latest history %s", currentAt, at)
			}
			i++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if i != len(want) {
			t.Fatalf("history count %d, want %d", i, len(want))
		}
	}
	checkHistory(tender.BankDepositMatched, tender.BankDepositProviderUnconfirmed)
	got, err := engine.ReconcileBankDeposit(t.Context(), "report-1", "payout-1", evidence)
	if err != nil || got.Outcome != tender.BankDepositProviderUnconfirmed {
		t.Fatalf("bank replay after demotion: %+v, %v", got, err)
	}
	checkHistory(tender.BankDepositMatched, tender.BankDepositProviderUnconfirmed)
	if err := store.Transact(t.Context(), func(tx tender.Tx) error {
		return tx.(tender.ReconciliationTx).AppendPayoutReconciliationResult(scope, "adyen", "acct", "report-1", initial)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT outcome FROM tender_bank_deposit_reviews WHERE report_id='report-1' AND payout_id='payout-1'`).Scan(&currentOutcome); err != nil {
		t.Fatal(err)
	}
	if currentOutcome != string(tender.BankDepositProviderUnconfirmed) {
		t.Fatalf("provider restoration auto-matched bank review: %s", currentOutcome)
	}
	got, err = engine.ReconcileBankDeposit(t.Context(), "report-1", "payout-1", evidence)
	if err != nil || got.Outcome != tender.BankDepositMatched {
		t.Fatalf("explicit bank replay after restoration: %+v, %v", got, err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT recorded_at FROM tender_bank_deposit_reviews WHERE report_id='report-1' AND payout_id='payout-1'`).Scan(&currentAt); err != nil {
		t.Fatal(err)
	}
	checkHistory(tender.BankDepositMatched, tender.BankDepositProviderUnconfirmed, tender.BankDepositMatched)
}

func TestBankDepositCrossScopeIsolation(t *testing.T) {
	_, db, engine, scope, first := bankDepositFixture(t)
	if got, err := engine.ReconcileBankDeposit(t.Context(), "report-1", "payout-1", first); err != nil || got.Outcome != tender.BankDepositMatched {
		t.Fatalf("first scope: %+v, %v", got, err)
	}
	other := scope
	other.MerchantID = "other-deposit-shop"
	otherEngine, err := tender.NewEngine(tender.EngineConfig{Store: postgres.New(db), Provider: storageProvider{}, Scope: other, ProviderName: "adyen", MerchantAccount: "acct", Market: tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}})
	if err != nil {
		t.Fatal(err)
	}
	seedAdditionalBankPayout(t, db, otherEngine, other, "2", "TX123XT")
	independent := first
	independent.Scope = other
	independent.AmountMinor = 5000
	if got, err := otherEngine.ReconcileBankDeposit(t.Context(), "report-2", "payout-2", independent); err != nil || got.Outcome != tender.BankDepositMatched {
		t.Fatalf("independent scope: %+v, %v", got, err)
	}
	if _, err := engine.ReconcileBankDeposit(t.Context(), "report-2", "payout-2", independent); !errors.Is(err, tender.ErrValidation) {
		t.Fatalf("cross-scope evidence entered original engine: %v", err)
	}
}

func TestBankDepositDownMigrationOrder(t *testing.T) {
	_, db, engine, _, evidence := bankDepositFixture(t)
	if got, err := engine.ReconcileBankDeposit(t.Context(), "report-1", "payout-1", evidence); err != nil || got.Outcome != tender.BankDepositMatched {
		t.Fatalf("seed bank claim: %+v, %v", got, err)
	}
	for _, name := range []string{"008_bank_deposits.down.sql", "007_return_evidence_claims.down.sql", "006_adverse_review.down.sql", "005_payout_batches.down.sql"} {
		script, err := postgres.Migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), string(script)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
