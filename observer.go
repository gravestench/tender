package tender

import (
	"context"
	"errors"
	"time"
)

// OperationalKind is a bounded stage name. It never contains provider data.
type OperationalKind string

const (
	OperationalProviderCall   OperationalKind = "provider_call"
	OperationalWebhookReceipt OperationalKind = "webhook_receipt"
	OperationalReconciliation OperationalKind = "reconciliation"
)

// OperationalEvent is a privacy-limited observation of committed work. IDs are
// host-assigned, opaque and scoped. No request, provider response or report row
// is carried. ReceiptLag is measured from the persisted inbox receive time;
// zero means that the store does not expose that time.
type OperationalEvent struct {
	Kind                               OperationalKind
	At                                 time.Time
	Scope                              Scope
	Provider, MerchantAccount          string
	PaymentID, OperationID             string
	Duration, ReceiptLag               time.Duration
	ErrorKind                          ErrorKind
	Discrepancies, PayoutDiscrepancies int
}

// Observer is optional. Implementations should return promptly and avoid
// network calls. An error or panic is ignored by the financial operation.
type Observer interface {
	Observe(context.Context, OperationalEvent) error
}

func observedErrorKind(err error) ErrorKind {
	if err == nil {
		return ""
	}
	for _, kind := range []ErrorKind{ErrUnknownOutcome, ErrRateLimit, ErrDeclined, ErrValidation, ErrAuthentication, ErrUnsupported, ErrNotDispatched, ErrConflict, ErrTransport} {
		if errors.Is(err, kind) {
			return kind
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrUnknownOutcome
	}
	return ErrTransport
}

func (e *Engine) observe(ctx context.Context, event OperationalEvent) {
	if e == nil || e.config.Observer == nil {
		return
	}
	event.At = e.config.Now().UTC()
	if event.Duration < 0 {
		event.Duration = 0
	}
	if event.ReceiptLag < 0 {
		event.ReceiptLag = 0
	}
	event.Scope = e.config.Scope
	event.Provider = e.config.ProviderName
	event.MerchantAccount = e.config.MerchantAccount
	defer func() { _ = recover() }()
	_ = e.config.Observer.Observe(context.WithoutCancel(ctx), event)
}

func verifiedReceiptTime(tx Tx, key VerifiedEventKey) time.Time {
	reader, ok := tx.(VerifiedReceiptTimeReader)
	if !ok {
		return time.Time{}
	}
	at, err := reader.VerifiedEventReceivedAt(key)
	if err != nil {
		return time.Time{}
	}
	return at
}

func (e *Engine) observeReceipt(ctx context.Context, started time.Time, receivedAt time.Time, paymentID, operationID string) {
	lag := time.Duration(0)
	if !receivedAt.IsZero() {
		lag = max(0, e.config.Now().UTC().Sub(receivedAt))
	}
	e.observe(ctx, OperationalEvent{Kind: OperationalWebhookReceipt, PaymentID: paymentID, OperationID: operationID, Duration: time.Since(started), ReceiptLag: lag})
}
