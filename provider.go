package tender

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type Environment string

const (
	EnvironmentMock    Environment = "MOCK"
	EnvironmentSandbox Environment = "SANDBOX"
	EnvironmentLive    Environment = "LIVE"
)

// Scope identifies the host merchant and environment across storage and calls.
type Scope struct {
	MerchantID  string
	Environment Environment
}

func (s Scope) Validate() error {
	if strings.TrimSpace(s.MerchantID) == "" {
		return fmt.Errorf("%w: missing merchant", ErrValidation)
	}
	switch s.Environment {
	case EnvironmentMock, EnvironmentSandbox, EnvironmentLive:
		return nil
	default:
		return fmt.Errorf("%w: invalid environment", ErrValidation)
	}
}

type ActionMode string

const (
	ActionComponent ActionMode = "component"
	ActionFrame     ActionMode = "frame"
	ActionModal     ActionMode = "modal"
	ActionPopup     ActionMode = "popup"
	ActionRedirect  ActionMode = "redirect"
)

// CustomerAction contains browser-safe opaque provider data. The consumer
// checks the mode against its navigation policy before exposing the action.
type CustomerAction struct {
	Mode      ActionMode
	Data      string
	ExpiresAt time.Time
}

func (a CustomerAction) Validate(at time.Time) error {
	if a.Data == "" || at.IsZero() || !a.ExpiresAt.After(at) {
		return fmt.Errorf("%w: missing or expired customer action", ErrValidation)
	}
	switch a.Mode {
	case ActionComponent, ActionFrame, ActionModal, ActionPopup, ActionRedirect:
		return nil
	default:
		return fmt.Errorf("%w: unknown customer action mode", ErrValidation)
	}
}

type OperationKind string

const (
	OpCreate  OperationKind = "create"
	OpConfirm OperationKind = "confirm"
	OpCapture OperationKind = "capture"
	OpVoid    OperationKind = "void"
	OpRefund  OperationKind = "refund"
)

type Capabilities struct {
	Create, Confirm, Capture, Void, Refund bool
	Query                                  bool
	ActionModes                            []ActionMode
}

// Supports reports whether the adapter implements a particular mutation.
func (c Capabilities) Supports(kind OperationKind) bool {
	switch kind {
	case OpCreate:
		return c.Create
	case OpConfirm:
		return c.Confirm
	case OpCapture:
		return c.Capture
	case OpVoid:
		return c.Void
	case OpRefund:
		return c.Refund
	default:
		return false
	}
}

// ProviderRequest carries the durable reservation and the current payment.
// Token is transient opaque input for Execute. It may be absent during Query.
type ProviderRequest struct {
	Payment   Payment
	Operation Operation
	Token     string
}

// Observation is an adapter's normalized result. Amount is the observed
// operation amount. Snapshot holds cumulative facts in the payment currency.
// The engine binds references and checks authority before applying a snapshot.
type Observation struct {
	Scope                                 Scope
	MerchantAccount                       string
	PaymentID, AttemptID, OperationID     string
	ProviderReference, OperationReference string
	Amount                                Money
	Status                                OperationStatus
	Snapshot                              Snapshot
}

// Provider performs contextual mutations and authoritative recovery queries.
// Query must not initiate a new payment. Unsupported operations return
// ErrUnsupported. An ambiguous dispatch returns ErrUnknownOutcome.
type Provider interface {
	Capabilities() Capabilities
	Execute(context.Context, ProviderRequest) (Observation, error)
	Query(context.Context, ProviderRequest) (Observation, error)
}
