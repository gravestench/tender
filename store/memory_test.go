package store_test

import (
	"errors"
	"testing"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/store"
)

func TestMemoryTransactionRollback(t *testing.T) {
	db := store.NewMemory()
	scope := tender.Scope{MerchantID: "shop", Environment: tender.EnvironmentMock}
	p, err := tender.NewPayment(scope, "payment", "order", tender.Money{Minor: 100, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	abort := errors.New("simulated commit failure")
	err = db.Transact(t.Context(), func(tx tender.Tx) error {
		if err := tx.InsertPayment(p); err != nil {
			return err
		}
		if err := tx.AppendAudit(tender.AuditEntry{Scope: scope, PaymentID: p.ID}); err != nil {
			return err
		}
		if err := tx.Enqueue(tender.OutboxEntry{Scope: scope, ID: p.ID, PaymentID: p.ID}); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	h, err := db.History(t.Context(), scope, p.ID)
	if err != nil || len(h) != 0 {
		t.Fatalf("uncommitted history: %v, %v", h, err)
	}
	o, err := db.Outbox(t.Context(), scope)
	if err != nil || len(o) != 0 {
		t.Fatalf("uncommitted outbox: %v, %v", o, err)
	}
	err = db.Transact(t.Context(), func(tx tender.Tx) error { _, err := tx.Payment(scope, p.ID); return err })
	if !errors.Is(err, tender.ErrNotFound) {
		t.Fatalf("uncommitted payment: %v", err)
	}
}

func TestMemoryImmutableReservation(t *testing.T) {
	db := store.NewMemory()
	scope := tender.Scope{MerchantID: "shop", Environment: tender.EnvironmentMock}
	p, err := tender.NewPayment(scope, "p", "order", tender.Money{Minor: 100, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	p.Provider = "acquirer"
	p.MerchantAccount = "account"
	p.AttemptID = "attempt"
	op := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: "op", PaymentID: "p", AttemptID: "attempt", Key: "key", Kind: tender.OpCreate, Status: tender.OperationReserved, Fingerprint: "fingerprint", ProviderKey: "provider-key", Amount: p.Amount, BaseVersion: p.Version}, PaymentVersion: p.Version}
	err = db.Transact(t.Context(), func(tx tender.Tx) error {
		if err := tx.InsertPayment(p); err != nil {
			return err
		}
		return tx.InsertOperation(op)
	})
	if err != nil {
		t.Fatal(err)
	}
	changed := p
	changed.MerchantAccount = "different"
	changed.Version++
	err = db.Transact(t.Context(), func(tx tender.Tx) error { return tx.UpdatePayment(changed) })
	if !errors.Is(err, tender.ErrConflict) {
		t.Errorf("overwrote merchant account: %v", err)
	}
	op.ProviderKey = "different"
	err = db.Transact(t.Context(), func(tx tender.Tx) error { return tx.UpdateOperation(op) })
	if !errors.Is(err, tender.ErrConflict) {
		t.Errorf("overwrote reservation's provider key: %v", err)
	}
}
