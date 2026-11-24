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

// PaymentMethod identifies a checkout rail or network.
type PaymentMethod string

const (
	MethodVisa       PaymentMethod = "visa"
	MethodMastercard PaymentMethod = "mastercard"
	MethodPayPal     PaymentMethod = "paypal"
	MethodKlarna     PaymentMethod = "klarna"
	MethodACH        PaymentMethod = "ach"
)

// Market is an exact combination of merchant country (ISO 3166-1 alpha-2),
// payment currency (ISO 4217), and checkout method.
type Market struct {
	Country, Currency string
	Method            PaymentMethod
}

// MarketCapability advertises one verified market and its partial operations.
type MarketCapability struct {
	Market                        Market
	PartialCapture, PartialRefund bool
}

type Capabilities struct {
	Create, Confirm, Capture, Void, Refund bool
	Query                                  bool
	ActionModes                            []ActionMode
	Markets                                []MarketCapability
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

// SupportsIn checks a mutation against an explicitly advertised combination.
// Only capture and refund can be requested as partial operations.
func (c Capabilities) SupportsIn(kind OperationKind, market Market, partial bool) bool {
	if !c.Supports(kind) || len(market.Country) != 2 || market.Method == "" {
		return false
	}
	if _, err := CurrencyExponent(market.Currency); err != nil {
		return false
	}
	for _, entry := range c.Markets {
		if entry.Market == market {
			if !partial || kind == OpCapture && entry.PartialCapture || kind == OpRefund && entry.PartialRefund {
				return true
			}
		}
	}
	return false
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
