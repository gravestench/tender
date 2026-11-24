package tender

import (
	"context"
	"encoding/json"
	"time"
)

// ErrNotFound indicates a missing scoped record or index entry.
const ErrNotFound ErrorKind = "not_found"

// OperationRecord retains the last payment version authoritative for this
// reservation and the definitive error category returned on an exact replay.
type OperationRecord struct {
	Operation
	PaymentVersion uint64
	// DispatchUntil leases one same-key HTTP call on replayable operations.
	// A recovery takeover marks the operation unknown before leaving storage.
	DispatchUntil time.Time
	Failure       ErrorKind
	FailureStatus string
	RetryAfter    time.Duration
	// RetryNotBefore survives restarts. SuccessorID is set at most once when
	// a definitive no-effect rejection permits a new reservation on the claim.
	RetryNotBefore time.Time
	SuccessorID    string
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

// VerifiedEventKey is the provider-defined delivery identity on one engine route.
type VerifiedEventKey struct {
	Scope                         Scope
	Provider, MerchantAccount, ID string
}

// VerifiedEvent is normalized from persisted authenticated evidence. ID and
// Provider identify the delivery. Observation carries its account and scope.
type VerifiedEvent struct {
	ID, Provider string
	Observation  Observation
}

// VerifiedEventTx extends Tx for stores with a verified callback inbox. Insert
// accepts allowlisted authenticated JSON and returns ErrConflict on a duplicate
// key. Process runs fn against that stored evidence, recording success in the
// same transaction, or returns nil for an already processed delivery. Return
// every processing error from the outer Transact callback so it rolls back.
type VerifiedEventTx interface {
	InsertVerifiedEvent(VerifiedEventKey, json.RawMessage) error
	ProcessVerifiedEvent(VerifiedEventKey, time.Time, func(json.RawMessage) error) error
}

// CaptureFailureNotice is a host review item for a processor's rejection of
// a previously submitted capture. Its scope and operation ID deduplicate
// delivery independently of the full-collection outbox.
type CaptureFailureNotice struct {
	Scope                                                      Scope
	Provider, MerchantAccount, PaymentID, OrderID, OperationID string
	ProviderReference, OperationReference                      string
	Amount                                                     Money
	At                                                         time.Time
}

type CaptureFailureTx interface {
	EnqueueCaptureFailure(CaptureFailureNotice) error
}

// VerifiedEvidenceReader returns allowlisted evidence containing the supplied
// top-level string fields, including already processed events on this route.
type VerifiedEvidenceReader interface {
	MatchingVerifiedEvidence(Scope, string, string, map[string]string) ([]json.RawMessage, error)
}
