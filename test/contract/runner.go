// Package contract compares fixture operations with the corresponding durable
// host records in offline provider contract tests.
package contract

import (
	"context"
	"errors"
	"fmt"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
)

// Records contains a fixture snapshot and the host operation and payment read
// together. Provider-specific checks interpret the fixture's wire data.
type Records struct {
	Fixture   mockserver.Record
	Payment   tender.Payment
	Operation tender.OperationRecord
}

// Verify loads both sides after the caller has synchronized the operation and
// any relevant callbacks. The check runs outside the host transaction.
func Verify(ctx context.Context, s *mockserver.Server, store tender.Store, scope tender.Scope, operationID, fixtureKey string, check func(Records) error) error {
	if check == nil {
		return errors.New("contract: missing comparison")
	}
	rec, ok := s.Lookup(fixtureKey)
	if !ok {
		return fmt.Errorf("contract: fixture record %q missing", fixtureKey)
	}
	records := Records{Fixture: rec}
	if err := store.Transact(ctx, func(tx tender.Tx) error {
		var err error
		records.Operation, err = tx.Operation(scope, operationID)
		if err != nil {
			return err
		}
		records.Payment, err = tx.Payment(scope, records.Operation.PaymentID)
		return err
	}); err != nil {
		return fmt.Errorf("contract: host records: %w", err)
	}
	return check(records)
}
