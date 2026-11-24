package tender

import (
	"fmt"
	"math"
	"strings"
	"time"
)

type PaymentStatus string

const (
	PaymentCreated        PaymentStatus = "created"
	PaymentPending        PaymentStatus = "pending"
	PaymentRequiresAction PaymentStatus = "requires_action"
	PaymentAuthorized     PaymentStatus = "authorized"
	PaymentCaptured       PaymentStatus = "captured"
	PaymentSettled        PaymentStatus = "settled"
	PaymentReturned       PaymentStatus = "returned"
	PaymentDeclined       PaymentStatus = "declined"
	PaymentVoided         PaymentStatus = "voided"
	PaymentFailed         PaymentStatus = "failed"
)

// Funds records cumulative gross amounts in the payment currency. Refunds,
// disputes and returns remain separate from the original capture or settlement.
// A bank payment may settle without a card authorization or capture.
type Funds struct {
	Authorized, Captured, Refunded, Settled, Returned, Disputed int64
}

func (f Funds) within(limit int64) bool {
	for _, n := range [...]int64{f.Authorized, f.Captured, f.Refunded, f.Settled, f.Returned, f.Disputed} {
		if n < 0 || n > limit {
			return false
		}
	}
	return f.Captured <= f.Authorized && f.Refunded <= f.Captured
}

func (f Funds) includes(previous Funds) bool {
	return f.Authorized >= previous.Authorized && f.Captured >= previous.Captured &&
		f.Refunded >= previous.Refunded && f.Settled >= previous.Settled &&
		f.Returned >= previous.Returned && f.Disputed >= previous.Disputed
}

// Snapshot is an absolute provider observation. An absent action has its zero
// value. Funds are cumulative even when a particular operation is partial.
type Snapshot struct {
	Status         PaymentStatus
	Funds          Funds
	Action         CustomerAction
	ProviderStatus string
}

func (s Snapshot) validate(limit int64, at time.Time) error {
	if !s.Funds.within(limit) {
		return fmt.Errorf("%w: inconsistent financial totals", ErrValidation)
	}
	if s.Status == PaymentRequiresAction {
		if err := s.Action.Validate(at); err != nil {
			return err
		}
	} else if s.Action != (CustomerAction{}) {
		return fmt.Errorf("%w: unexpected customer action", ErrValidation)
	}
	var valid bool
	switch s.Status {
	case PaymentCreated, PaymentPending, PaymentRequiresAction, PaymentDeclined, PaymentFailed:
		valid = s.Funds == (Funds{})
	case PaymentAuthorized:
		valid = s.Funds.Authorized > 0 && s.Funds.Captured == 0
	case PaymentCaptured:
		valid = s.Funds.Captured > 0
	case PaymentSettled:
		valid = s.Funds.Settled > 0
	case PaymentReturned:
		valid = s.Funds.Returned > 0
	case PaymentVoided:
		valid = s.Funds.Captured == 0 || s.Funds.Captured < s.Funds.Authorized
	}
	if !valid {
		return fmt.Errorf("%w: inconsistent payment status", ErrValidation)
	}
	return nil
}

func (s Snapshot) canAdvance(next Snapshot) bool {
	if !next.Funds.includes(s.Funds) {
		return false
	}
	if s.Status == PaymentVoided {
		return next.Status == PaymentVoided && next.Funds.Authorized == s.Funds.Authorized && next.Funds.Captured == s.Funds.Captured
	}
	if s.Status == next.Status {
		return true
	}
	switch s.Status {
	case PaymentCreated:
		return true
	case PaymentPending, PaymentRequiresAction:
		return next.Status != PaymentCreated
	case PaymentAuthorized:
		return next.Status == PaymentCaptured || next.Status == PaymentSettled || next.Status == PaymentVoided
	case PaymentCaptured:
		return next.Status == PaymentSettled || next.Status == PaymentVoided || next.Status == PaymentReturned
	case PaymentSettled:
		return next.Status == PaymentReturned
	default:
		return false
	}
}

// Payment belongs to one scoped host order. ProviderReference is opaque and
// meaningful only for its provider and merchant account in this environment.
type Payment struct {
	Scope                                                   Scope
	ID, OrderID                                             string
	Provider, MerchantAccount, AttemptID, ProviderReference string
	Amount                                                  Money
	Version                                                 uint64
	Snapshot
}

func NewPayment(scope Scope, id, orderID string, amount Money) (Payment, error) {
	if err := scope.Validate(); err != nil {
		return Payment{}, err
	}
	if err := amount.ValidatePurchase(); err != nil {
		return Payment{}, err
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(orderID) == "" {
		return Payment{}, fmt.Errorf("%w: missing payment or order identity", ErrValidation)
	}
	return Payment{Scope: scope, ID: id, OrderID: orderID, Amount: amount, Version: 1, Snapshot: Snapshot{Status: PaymentCreated}}, nil
}

// Correction identifies the host-authorized actor and evidence for a revision.
type Correction struct {
	Actor, Reason string
}

// Change records a transition at its resulting version. Store it atomically
// with the payment so that ordinary changes and corrections remain auditable.
type Change struct {
	Before, After Snapshot
	At            time.Time
	Version       uint64
	Correction    Correction
}

func (p *Payment) Apply(next Snapshot, at time.Time) (Change, error) {
	if !p.Snapshot.canAdvance(next) {
		return Change{}, fmt.Errorf("%w: payment transition would reverse a recorded outcome", ErrValidation)
	}
	return p.record(next, at, Correction{})
}

// Correct can revise history only through a new version with an audit reason.
// The corrected snapshot still must satisfy financial and action invariants.
func (p *Payment) Correct(next Snapshot, at time.Time, basis Correction) (Change, error) {
	if strings.TrimSpace(basis.Actor) == "" || strings.TrimSpace(basis.Reason) == "" {
		return Change{}, fmt.Errorf("%w: correction requires actor and reason", ErrValidation)
	}
	return p.record(next, at, basis)
}

func (p *Payment) record(next Snapshot, at time.Time, basis Correction) (Change, error) {
	if p.Version == 0 || p.Version == math.MaxUint64 || at.IsZero() {
		return Change{}, fmt.Errorf("%w: invalid payment version or observation time", ErrValidation)
	}
	if err := p.Amount.ValidatePurchase(); err != nil {
		return Change{}, err
	}
	if err := next.validate(p.Amount.Minor, at); err != nil {
		return Change{}, err
	}
	next.ProviderStatus = sanitizeStatus(next.ProviderStatus)
	change := Change{Before: p.Snapshot, After: next, At: at, Version: p.Version + 1, Correction: basis}
	p.Snapshot, p.Version = next, change.Version
	return change, nil
}

type OperationStatus string

const (
	OperationReserved  OperationStatus = "reserved"
	OperationPending   OperationStatus = "pending"
	OperationUnknown   OperationStatus = "unknown"
	OperationSucceeded OperationStatus = "succeeded"
	OperationFailed    OperationStatus = "failed"
)

// Operation holds the host and provider keys across retries and restarts.
// BaseVersion is the payment version seen when the operation was reserved.
type Operation struct {
	Scope                                            Scope
	ID, PaymentID, AttemptID                         string
	Key, Fingerprint, ProviderKey, ProviderReference string
	Kind                                             OperationKind
	Status                                           OperationStatus
	Amount                                           Money
	BaseVersion                                      uint64
}

// SetStatus records an operation outcome independently of payment state. Only
// an authoritative result should resolve an uncertain or pending operation.
func (op *Operation) SetStatus(next OperationStatus) error {
	switch next {
	case OperationPending, OperationUnknown, OperationSucceeded, OperationFailed:
	default:
		return fmt.Errorf("%w: invalid operation outcome", ErrValidation)
	}
	switch op.Status {
	case OperationReserved, OperationPending, OperationUnknown:
	case OperationSucceeded, OperationFailed:
		if op.Status != next {
			return fmt.Errorf("%w: operation already completed", ErrConflict)
		}
	default:
		return fmt.Errorf("%w: invalid stored operation status", ErrValidation)
	}
	op.Status = next
	return nil
}
