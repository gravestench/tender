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

// Observer is optional. An error or panic is ignored by the financial operation.
type Observer interface {
	Observe(context.Context, OperationalEvent) error
}

const operationalObserverQueueSize = 64

// DroppedOperationalEvents counts newest events discarded when the bounded
// observer queue is full. It can be scraped by the host without invoking the
// observer itself.
func (e *Engine) DroppedOperationalEvents() uint64 {
	if e == nil {
		return 0
	}
	return e.observerDropped.Load()
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
	e.observerMu.Lock()
	if e.observerQueue == nil {
		e.observerQueue = make(chan OperationalEvent, operationalObserverQueueSize)
	}
	select {
	case e.observerQueue <- event:
		if !e.observerRunning {
			e.observerRunning = true
			go e.drainObserver()
		}
	default:
		e.observerDropped.Add(1)
	}
	e.observerMu.Unlock()
}

func (e *Engine) drainObserver() {
	for {
		event := <-e.observerQueue
		func() {
			defer func() { _ = recover() }()
			_ = e.config.Observer.Observe(context.Background(), event)
		}()
		e.observerMu.Lock()
		if len(e.observerQueue) == 0 {
			e.observerRunning = false
			e.observerMu.Unlock()
			return
		}
		e.observerMu.Unlock()
	}
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

// ObserveVerifiedReceipt is an adapter-worker bridge for an inbox marker that
// has just committed outside the Engine's own receipt methods. It accepts only
// scoped opaque IDs and a persisted receive timestamp, never receipt evidence.
func (e *Engine) ObserveVerifiedReceipt(ctx context.Context, started, receivedAt time.Time, paymentID, operationID string) {
	if e == nil || !boundedOpaque(paymentID, 200) || operationID != "" && !boundedOpaque(operationID, 200) {
		return
	}
	e.observeReceipt(ctx, started, receivedAt, paymentID, operationID)
}

func (e *Engine) observeReceipt(ctx context.Context, started time.Time, receivedAt time.Time, paymentID, operationID string) {
	lag := time.Duration(0)
	if !receivedAt.IsZero() {
		lag = max(0, e.config.Now().UTC().Sub(receivedAt))
	}
	e.observe(ctx, OperationalEvent{Kind: OperationalWebhookReceipt, PaymentID: paymentID, OperationID: operationID, Duration: time.Since(started), ReceiptLag: lag})
}
