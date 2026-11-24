package tender

import (
	"context"
	"time"
)

// ErrNotFound indicates a missing scoped record or index entry.
const ErrNotFound ErrorKind = "not_found"

// OperationRecord retains the last payment version authoritative for this
// reservation and the definitive error category returned on an exact replay.
type OperationRecord struct {
	Operation
	PaymentVersion uint64
	Failure        ErrorKind
	FailureStatus  string
	RetryAfter     time.Duration
}

// AuditEntry records a reservation or outcome. A zero Change means only the
// operation changed. At and Source identify how the record was produced.
type AuditEntry struct {
	Scope                  Scope
	PaymentID, OperationID string
	OperationStatus        OperationStatus
	At                     time.Time
	Source                 string
	Change                 Change
}

// OutboxEntry is the first observation of full collection, in the same
// transaction as the payment. The host checks current state and its own
// fulfillment policy and consumes the scoped ID idempotently.
type OutboxEntry struct {
	Scope                               Scope
	ID, PaymentID, OrderID, OperationID string
	Version                             uint64
	At                                  time.Time
}

// Store runs fn once in a serializable transaction. All writes commit or
// none do. Implementations must serialize conflicting reads and writes across
// all workers, including absent keys, or return ErrConflict on a lost race.
// A commit error can be ambiguous. Callers can look up the original key.
type Store interface {
	Transact(context.Context, func(Tx) error) error
}

// Tx is valid only during its Store callback. Reads return ErrNotFound for
// absent records. Payment IDs, order claims, operation IDs, operation keys
// and active operations per payment are unique within Scope. Nonempty
// payment provider references are unique per scope, provider and account.
// Operation provider references are unique within a payment. Duplicate
// inserts fail with ErrConflict. UpdatePayment advances exactly one version,
// keeping the original order, attempt, route and purchase amount. Both update
// methods preserve bound references and durable request identity. Enqueue
// deduplicates by scoped entry ID.
type Tx interface {
	Payment(Scope, string) (Payment, error)
	PaymentByOrder(Scope, string) (Payment, error)
	Operation(Scope, string) (OperationRecord, error)
	OperationByKey(Scope, string) (OperationRecord, error)
	OperationByReference(scope Scope, paymentID, reference string) (OperationRecord, error)
	ActiveOperation(scope Scope, paymentID string) (OperationRecord, error)
	InsertPayment(Payment) error
	UpdatePayment(Payment) error
	InsertOperation(OperationRecord) error
	UpdateOperation(OperationRecord) error
	AppendAudit(AuditEntry) error
	Enqueue(OutboxEntry) error
}
