//go:build integration

package contract_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/store"
	"github.com/gravestench/tender/store/postgres"
)

type adverseStore interface {
	tender.Store
	AdverseReviews(context.Context, tender.Scope) ([]tender.AdverseNotice, error)
	ReviewAdverse(context.Context, tender.AdverseKey, tender.AdverseDecision) error
	Outbox(context.Context, tender.Scope) ([]tender.OutboxEntry, error)
}

type adverseEvidence struct {
	Provider, Account, EventID, PaymentID, OperationID, Original, Modification, Kind string
	Amount                                                                           int64
	Currency                                                                         string
}

func adverseProjection(_ tender.EventRecords, raw json.RawMessage) (tender.AdverseNotice, error) {
	var e adverseEvidence
	if err := json.Unmarshal(raw, &e); err != nil {
		return tender.AdverseNotice{}, err
	}
	return tender.AdverseNotice{
		Scope:    tender.Scope{MerchantID: "adverse-shop", Environment: tender.EnvironmentMock},
		Provider: e.Provider, MerchantAccount: e.Account, ProviderEventID: e.EventID,
		PaymentID: e.PaymentID, OperationID: e.OperationID, Kind: e.Kind, OriginalReference: e.Original,
		ModificationReference: e.Modification, Amount: tender.Money{Minor: e.Amount, Currency: e.Currency},
		ObservedAt: time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC),
	}, nil
}

func TestAdverseReviewDurability(t *testing.T) {
	ctx := t.Context()
	scope := tender.Scope{MerchantID: "adverse-shop", Environment: tender.EnvironmentMock}
	market := tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}
	provider := storageProvider{}
	for _, tc := range []struct {
		name string
		open func(*testing.T) func() adverseStore
	}{
		{"memory", func(*testing.T) func() adverseStore {
			m := store.NewMemory()
			return func() adverseStore { return m }
		}},
		{"postgres", func(t *testing.T) func() adverseStore {
			open := postgresFixture(t)
			var previous *sql.DB
			return func() adverseStore {
				if previous != nil {
					previous.Close()
				}
				previous = open()
				return postgres.New(previous)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			open := tc.open(t)
			db := open()
			p, err := tender.NewPayment(scope, "payment", "order", tender.Money{Minor: 10000, Currency: "USD"})
			if err != nil {
				t.Fatal(err)
			}
			p.Provider, p.MerchantAccount, p.Market, p.AttemptID, p.ProviderReference = "processor", "account", market, "attempt", "payment-psp"
			if _, err := p.Apply(tender.Snapshot{Status: tender.PaymentCaptured, VerifiedMethod: tender.MethodVisa, Funds: tender.Funds{Authorized: 10000, Captured: 10000}}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := db.Transact(ctx, func(tx tender.Tx) error { return tx.InsertPayment(p) }); err != nil {
				t.Fatal(err)
			}
			engine, err := tender.NewEngine(tender.EngineConfig{Store: db, Provider: provider, Scope: scope, ProviderName: "processor", MerchantAccount: "account", Market: market})
			if err != nil {
				t.Fatal(err)
			}
			evidence := adverseEvidence{Provider: "processor", Account: "account", EventID: "event-1", PaymentID: "payment", Original: "payment-psp", Modification: "dispute-psp", Kind: "chargeback", Amount: 5000, Currency: "USD"}
			insert := func(id string, e adverseEvidence) error {
				raw, _ := json.Marshal(e)
				return db.Transact(ctx, func(tx tender.Tx) error {
					return tx.(tender.VerifiedEventTx).InsertVerifiedEvent(tender.VerifiedEventKey{Scope: scope, Provider: "processor", MerchantAccount: "account", ID: id}, raw)
				})
			}
			if err := insert("event-1", evidence); err != nil {
				t.Fatal(err)
			}
			preRaw, _ := json.Marshal(evidence)
			preNotice, _ := adverseProjection(nil, preRaw)
			if err := db.Transact(ctx, func(tx tender.Tx) error {
				return tx.(tender.AdverseReviewTx).EnqueueAdverse(preNotice, tender.AdverseClaim{})
			}); !errors.Is(err, tender.ErrConflict) {
				t.Fatalf("direct unprocessed enqueue: %v", err)
			}
			if premature, err := db.AdverseReviews(ctx, scope); err != nil || len(premature) != 0 {
				t.Fatalf("unprocessed receipt exposed review item: %+v, %v", premature, err)
			}
			prematureDecision := tender.AdverseDecision{Disposition: "investigate", ReviewerID: "operator-1", ReviewedAt: time.Date(2026, 11, 25, 2, 0, 0, 0, time.UTC), Rationale: "await_provider_evidence"}
			if err := db.ReviewAdverse(ctx, preNotice.Key(), prematureDecision); !errors.Is(err, tender.ErrNotFound) {
				t.Fatalf("unprocessed receipt reviewed: %v", err)
			}
			if err := db.Transact(ctx, func(tx tender.Tx) error {
				rows, err := tx.(tender.AdverseEvidenceReader).AdverseEvidence(scope, "processor", "account", "payment")
				if err != nil || len(rows) != 0 {
					t.Fatalf("unprocessed receipt counted as evidence: %+v, %v", rows, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// A signed receipt plus notice and processed marker must commit once,
			// including concurrent redeliveries and after reopening the SQL pool.
			var wg sync.WaitGroup
			results := make(chan error, 4)
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					results <- engine.ProcessVerifiedAdverseEvent(context.Background(), "event-1", adverseProjection)
				}()
			}
			wg.Wait()
			close(results)
			for err := range results {
				if err != nil && !errors.Is(err, tender.ErrConflict) {
					t.Fatalf("concurrent process: %v", err)
				}
			}
			notices, err := db.AdverseReviews(ctx, scope)
			if err != nil || len(notices) != 1 || notices[0].Kind != "chargeback" || notices[0].ModificationReference != "dispute-psp" || notices[0].Decision != nil {
				t.Fatalf("notice after replay: %+v, %v", notices, err)
			}
			otherScope := tender.Scope{MerchantID: "another-shop", Environment: tender.EnvironmentMock}
			if foreign, err := db.AdverseReviews(ctx, otherScope); err != nil || len(foreign) != 0 {
				t.Fatalf("cross-merchant notice leak: %+v, %v", foreign, err)
			}
			if err := engine.ProcessVerifiedAdverseEvent(ctx, "event-1", adverseProjection); err != nil {
				t.Fatal(err)
			}
			if err := db.Transact(ctx, func(tx tender.Tx) error {
				rows, err := tx.(tender.AdverseEvidenceReader).AdverseEvidence(scope, "processor", "account", "payment")
				if err != nil || len(rows) != 1 || rows[0].OriginalReference != "payment-psp" || rows[0].ModificationReference != "dispute-psp" {
					t.Fatalf("processed receipt evidence: %+v, %v", rows, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := insert("event-1", evidence); !errors.Is(err, tender.ErrConflict) {
				t.Fatalf("duplicate receipt: %v", err)
			}
			if out, err := db.Outbox(ctx, scope); err != nil || len(out) != 0 {
				t.Fatalf("notice-only event changed outbox: %+v, %v", out, err)
			}
			changed := notices[0]
			changed.Kind = "different_event"
			if err := db.Transact(ctx, func(tx tender.Tx) error {
				return tx.(tender.AdverseReviewTx).EnqueueAdverse(changed, tender.AdverseClaim{})
			}); !errors.Is(err, tender.ErrConflict) {
				t.Fatalf("changed notice under existing event key: %v", err)
			}
			orphan := notices[0]
			orphan.ProviderEventID = "not-in-inbox"
			if err := db.Transact(ctx, func(tx tender.Tx) error {
				return tx.(tender.AdverseReviewTx).EnqueueAdverse(orphan, tender.AdverseClaim{})
			}); !errors.Is(err, tender.ErrConflict) {
				t.Fatalf("orphan notice accepted: %v", err)
			}
			if err := db.Transact(ctx, func(tx tender.Tx) error {
				got, err := tx.Payment(scope, p.ID)
				if err != nil {
					return err
				}
				if got.Version != p.Version || got.Funds != p.Funds {
					t.Fatalf("notice-only event changed funds: %+v", got)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			decision := tender.AdverseDecision{Disposition: "investigate", ReviewerID: "operator-1", ReviewedAt: time.Date(2026, 11, 25, 2, 0, 0, 0, time.UTC), Rationale: "await_provider_evidence"}
			key := tender.AdverseKey{Scope: scope, Provider: "processor", MerchantAccount: "account", ProviderEventID: "event-1"}
			if err := db.ReviewAdverse(ctx, key, decision); err != nil {
				t.Fatal(err)
			}
			if err := db.ReviewAdverse(ctx, key, decision); err != nil {
				t.Fatal(err)
			}
			if err := db.ReviewAdverse(ctx, key, tender.AdverseDecision{Disposition: "close", ReviewerID: "operator-2", ReviewedAt: decision.ReviewedAt, Rationale: "changed"}); !errors.Is(err, tender.ErrConflict) {
				t.Fatalf("changed review should conflict: %v", err)
			}
			notices, err = db.AdverseReviews(ctx, scope)
			if err != nil || len(notices) != 1 || notices[0].Decision == nil || *notices[0].Decision != decision {
				t.Fatalf("review transition: %+v, %v", notices, err)
			}
			if err := engine.ProcessVerifiedAdverseEvent(ctx, "event-1", adverseProjection); err != nil {
				t.Fatal(err)
			}
			if reopened := open(); reopened != nil {
				notices, err = reopened.AdverseReviews(ctx, scope)
				if err != nil || len(notices) != 1 || notices[0].Decision == nil {
					t.Fatalf("restart review: %+v, %v", notices, err)
				}
			}
		})
	}
}

func TestAdverseReviewRejectsWrongRoute(t *testing.T) {
	scope := tender.Scope{MerchantID: "adverse-shop", Environment: tender.EnvironmentMock}
	market := tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}
	db := store.NewMemory()
	p, _ := tender.NewPayment(scope, "payment", "order", tender.Money{Minor: 10000, Currency: "USD"})
	p.Provider, p.MerchantAccount, p.Market, p.AttemptID, p.ProviderReference = "processor", "account", market, "attempt", "payment-psp"
	if err := db.Transact(t.Context(), func(tx tender.Tx) error { return tx.InsertPayment(p) }); err != nil {
		t.Fatal(err)
	}
	other, _ := tender.NewPayment(scope, "other-payment", "other-order", tender.Money{Minor: 10000, Currency: "USD"})
	other.Provider, other.MerchantAccount, other.Market, other.AttemptID, other.ProviderReference = "processor", "account", market, "other-attempt", "other-psp"
	if err := db.Transact(t.Context(), func(tx tender.Tx) error { return tx.InsertPayment(other) }); err != nil {
		t.Fatal(err)
	}
	op := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: "capture-operation", PaymentID: p.ID, AttemptID: p.AttemptID, Key: "capture-key", Fingerprint: "fingerprint", ProviderKey: "provider-key", ProviderReference: "capture-psp", Kind: tender.OpCapture, Status: tender.OperationSucceeded, Amount: tender.Money{Minor: 5000, Currency: "USD"}}}
	if err := db.Transact(t.Context(), func(tx tender.Tx) error { return tx.InsertOperation(op) }); err != nil {
		t.Fatal(err)
	}
	unbound := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: "unbound-operation", PaymentID: p.ID, AttemptID: p.AttemptID, Key: "unbound-key", Fingerprint: "unbound-fingerprint", ProviderKey: "unbound-provider-key", Kind: tender.OpRefund, Status: tender.OperationSucceeded, Amount: tender.Money{Minor: 5000, Currency: "USD"}}}
	if err := db.Transact(t.Context(), func(tx tender.Tx) error { return tx.InsertOperation(unbound) }); err != nil {
		t.Fatal(err)
	}
	engine, _ := tender.NewEngine(tender.EngineConfig{Store: db, Provider: storageProvider{}, Scope: scope, ProviderName: "processor", MerchantAccount: "account", Market: market})
	for _, tc := range []struct {
		name   string
		change func(*adverseEvidence)
	}{
		{"account", func(e *adverseEvidence) { e.Account = "other-account" }},
		{"payment", func(e *adverseEvidence) { e.PaymentID = "other-payment" }},
		{"original", func(e *adverseEvidence) { e.Original = "wrong-original" }},
		{"swapped", func(e *adverseEvidence) { e.Original, e.Modification = e.Modification, e.Original }},
		{"amount", func(e *adverseEvidence) { e.Amount = 10001 }},
		{"operation", func(e *adverseEvidence) { e.OperationID = "capture-operation"; e.Modification = "wrong-capture-psp" }},
		{"unbound-operation", func(e *adverseEvidence) { e.OperationID = "unbound-operation" }},
		{"event", func(e *adverseEvidence) { e.EventID = "another-event" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := adverseEvidence{Provider: "processor", Account: "account", EventID: tc.name, PaymentID: "payment", Original: "payment-psp", Modification: "dispute-psp", Kind: "chargeback", Amount: 5000, Currency: "USD"}
			tc.change(&e)
			raw, _ := json.Marshal(e)
			key := tender.VerifiedEventKey{Scope: scope, Provider: "processor", MerchantAccount: "account", ID: tc.name}
			if err := db.Transact(t.Context(), func(tx tender.Tx) error { return tx.(tender.VerifiedEventTx).InsertVerifiedEvent(key, raw) }); err != nil {
				t.Fatal(err)
			}
			if err := engine.ProcessVerifiedAdverseEvent(t.Context(), tc.name, adverseProjection); err == nil {
				t.Fatal("mismatched evidence accepted")
			}
		})
	}
	if notices, err := db.AdverseReviews(t.Context(), scope); err != nil || len(notices) != 0 {
		t.Fatalf("mismatch created notices: %+v, %v", notices, err)
	}
}

func TestAdverseReviewRejectsDirectPostProcessing(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(*testing.T) adverseStore
	}{
		{"memory", func(*testing.T) adverseStore { return store.NewMemory() }},
		{"postgres", func(t *testing.T) adverseStore { open := postgresFixture(t); return postgres.New(open()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.open(t)
			ctx := t.Context()
			scope := tender.Scope{MerchantID: "adverse-shop", Environment: tender.EnvironmentMock}
			market := tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}
			for _, seed := range []struct{ id, ref string }{{"payment", "payment-psp"}, {"other-payment", "other-psp"}} {
				p, _ := tender.NewPayment(scope, seed.id, "order-"+seed.id, tender.Money{Minor: 10000, Currency: "USD"})
				p.Provider, p.MerchantAccount, p.Market, p.AttemptID, p.ProviderReference = "processor", "account", market, "attempt-"+seed.id, seed.ref
				if err := db.Transact(ctx, func(tx tender.Tx) error { return tx.InsertPayment(p) }); err != nil {
					t.Fatal(err)
				}
			}
			for _, scenario := range []struct {
				name  string
				alter func(*tender.AdverseNotice)
			}{
				{"exact", func(*tender.AdverseNotice) {}},
				{"other-payment", func(n *tender.AdverseNotice) { n.PaymentID = "other-payment"; n.OriginalReference = "other-psp" }},
				{"wrong-original", func(n *tender.AdverseNotice) { n.OriginalReference = "other-psp" }},
				{"wrong-amount", func(n *tender.AdverseNotice) { n.Amount.Minor = 9999 }},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					e := adverseEvidence{Provider: "processor", Account: "account", EventID: scenario.name, PaymentID: "payment", Original: "payment-psp", Modification: "dispute-" + scenario.name, Kind: "chargeback", Amount: 5000, Currency: "USD"}
					raw, _ := json.Marshal(e)
					key := tender.VerifiedEventKey{Scope: scope, Provider: "processor", MerchantAccount: "account", ID: scenario.name}
					if err := db.Transact(ctx, func(tx tender.Tx) error {
						inbox := tx.(tender.VerifiedEventTx)
						if err := inbox.InsertVerifiedEvent(key, raw); err != nil {
							return err
						}
						return inbox.ProcessVerifiedEvent(key, time.Now().UTC(), func(json.RawMessage) error { return nil })
					}); err != nil {
						t.Fatal(err)
					}
					n, _ := adverseProjection(nil, raw)
					scenario.alter(&n)
					if err := db.Transact(ctx, func(tx tender.Tx) error { return tx.(tender.AdverseReviewTx).EnqueueAdverse(n, tender.AdverseClaim{}) }); !errors.Is(err, tender.ErrConflict) {
						t.Fatalf("direct forged enqueue accepted: %v", err)
					}
				})
			}
			if reviews, err := db.AdverseReviews(ctx, scope); err != nil || len(reviews) != 0 {
				t.Fatalf("direct enqueue leaked reviews: %+v, %v", reviews, err)
			}
			if err := db.Transact(ctx, func(tx tender.Tx) error {
				rows, err := tx.(tender.AdverseEvidenceReader).AdverseEvidence(scope, "processor", "account", "payment")
				if err != nil || len(rows) != 0 {
					t.Fatalf("direct enqueue became evidence: %+v, %v", rows, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAdverseReviewAtomicRollback(t *testing.T) {
	open := postgresFixture(t)
	db := open()
	defer db.Close()
	ctx := t.Context()
	scope := tender.Scope{MerchantID: "adverse-shop", Environment: tender.EnvironmentMock}
	market := tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}
	p, _ := tender.NewPayment(scope, "payment", "order", tender.Money{Minor: 10000, Currency: "USD"})
	p.Provider, p.MerchantAccount, p.Market, p.AttemptID, p.ProviderReference = "processor", "account", market, "attempt", "payment-psp"
	s := postgres.New(db)
	if err := s.Transact(ctx, func(tx tender.Tx) error { return tx.InsertPayment(p) }); err != nil {
		t.Fatal(err)
	}
	engine, _ := tender.NewEngine(tender.EngineConfig{Store: s, Provider: storageProvider{}, Scope: scope, ProviderName: "processor", MerchantAccount: "account", Market: market})
	insert := func(id string) {
		t.Helper()
		e := adverseEvidence{Provider: "processor", Account: "account", EventID: id, PaymentID: "payment", Original: "payment-psp", Modification: "dispute-" + id, Kind: "chargeback", Amount: 5000, Currency: "USD"}
		raw, _ := json.Marshal(e)
		if err := s.Transact(ctx, func(tx tender.Tx) error {
			return tx.(tender.VerifiedEventTx).InsertVerifiedEvent(tender.VerifiedEventKey{Scope: scope, Provider: "processor", MerchantAccount: "account", ID: id}, raw)
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertRetryable := func(id string) {
		t.Helper()
		var count int
		var processed bool
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM tender_adverse_reviews WHERE merchant_id=$1 AND event_id=$2`, scope.MerchantID, id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("failure committed %d notices", count)
		}
		if err := db.QueryRowContext(ctx, `SELECT processed_at IS NOT NULL FROM tender_provider_events WHERE merchant_id=$1 AND event_id=$2`, scope.MerchantID, id).Scan(&processed); err != nil {
			t.Fatal(err)
		}
		if processed {
			t.Fatal("failure marked receipt processed")
		}
	}
	insert("reject-insert")
	if _, err := db.ExecContext(ctx, `ALTER TABLE tender_adverse_reviews ADD CONSTRAINT reject_notice CHECK (false)`); err != nil {
		t.Fatal(err)
	}
	if err := engine.ProcessVerifiedAdverseEvent(ctx, "reject-insert", adverseProjection); err == nil {
		t.Fatal("notice write failure ignored")
	}
	assertRetryable("reject-insert")
	if _, err := db.ExecContext(ctx, `ALTER TABLE tender_adverse_reviews DROP CONSTRAINT reject_notice`); err != nil {
		t.Fatal(err)
	}
	if err := engine.ProcessVerifiedAdverseEvent(ctx, "reject-insert", adverseProjection); err != nil {
		t.Fatal(err)
	}
	insert("reject-marker")
	if _, err := db.ExecContext(ctx, `ALTER TABLE tender_provider_events ADD CONSTRAINT reject_marker CHECK (processed_at IS NULL) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if err := engine.ProcessVerifiedAdverseEvent(ctx, "reject-marker", adverseProjection); err == nil {
		t.Fatal("processed-marker failure ignored")
	}
	assertRetryable("reject-marker")
	if _, err := db.ExecContext(ctx, `ALTER TABLE tender_provider_events DROP CONSTRAINT reject_marker`); err != nil {
		t.Fatal(err)
	}
	if err := engine.ProcessVerifiedAdverseEvent(ctx, "reject-marker", adverseProjection); err != nil {
		t.Fatal(err)
	}
	notices, err := s.AdverseReviews(ctx, scope)
	if err != nil || len(notices) != 2 {
		t.Fatalf("retry notices: %+v, %v", notices, err)
	}
	key := tender.AdverseKey{Scope: scope, Provider: "processor", MerchantAccount: "account", ProviderEventID: "reject-marker"}
	decision := tender.AdverseDecision{Disposition: "investigate", ReviewerID: "operator-1", ReviewedAt: time.Date(2026, 11, 25, 2, 0, 0, 0, time.UTC), Rationale: "verified_dispute"}
	if err := s.ReviewAdverse(ctx, key, decision); err != nil {
		t.Fatal(err)
	}
	if err := s.ReviewAdverse(ctx, key, decision); err != nil {
		t.Fatal(err)
	}
	var history int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM tender_adverse_review_history WHERE merchant_id=$1 AND event_id=$2`, scope.MerchantID, key.ProviderEventID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if history != 1 {
		t.Fatalf("review history count: %d", history)
	}
}

func TestAdverseMigrationRoundTrip(t *testing.T) {
	open := postgresFixture(t)
	db := open()
	defer db.Close()
	// 007 has a foreign key into 006, so roll back and restore both in
	// dependency order after postgresFixture has applied every migration.
	for _, path := range []string{
		"migrations/007_return_evidence_claims.down.sql",
		"migrations/006_adverse_review.down.sql",
		"migrations/006_adverse_review.up.sql",
		"migrations/007_return_evidence_claims.up.sql",
	} {
		migration, err := postgres.Migrations.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), string(migration)); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	var table string
	if err := db.QueryRowContext(t.Context(), `SELECT to_regclass('tender_adverse_reviews')::text`).Scan(&table); err != nil || table != "tender_adverse_reviews" {
		t.Fatalf("migration round trip left table %q: %v", table, err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT to_regclass('tender_reconciliation_return_claims')::text`).Scan(&table); err != nil || table != "tender_reconciliation_return_claims" {
		t.Fatalf("migration round trip left return claims table %q: %v", table, err)
	}
}
