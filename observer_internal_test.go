package tender

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type internalObserverFunc func(context.Context, OperationalEvent) error

func (f internalObserverFunc) Observe(ctx context.Context, e OperationalEvent) error {
	return f(ctx, e)
}

func TestOperationalDiscrepancyCounts(t *testing.T) {
	run := ReconciliationRun{Results: []ReconciliationResult{{Outcome: ReconciliationMatched}, {Outcome: ReconciliationUnmatched}}, PayoutResults: []PayoutReconciliationResult{{Outcome: PayoutMatched}, {Outcome: PayoutMissing}}}
	lines, payouts := operationalDiscrepancyCounts(run)
	if lines != 1 || payouts != 1 {
		t.Fatalf("discrepancies = %d, %d", lines, payouts)
	}
}

func TestOperationalObserverQueueIsBounded(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 1)
	e := &Engine{config: EngineConfig{Observer: internalObserverFunc(func(context.Context, OperationalEvent) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	}), Now: time.Now}}
	e.observe(t.Context(), OperationalEvent{Kind: OperationalProviderCall})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("observer worker did not start")
	}
	for range 255 {
		e.observe(t.Context(), OperationalEvent{Kind: OperationalProviderCall})
	}
	if got := e.DroppedOperationalEvents(); got != 191 {
		t.Fatalf("dropped %d, want 191 with one in flight and 64 queued", got)
	}
}

func TestOperationalObserverWorkerContinuesAfterPanic(t *testing.T) {
	var calls atomic.Int32
	delivered := make(chan OperationalEvent, 1)
	e := &Engine{config: EngineConfig{Observer: internalObserverFunc(func(_ context.Context, event OperationalEvent) error {
		if calls.Add(1) == 1 {
			panic("host sink failed")
		}
		delivered <- event
		return nil
	}), Now: time.Now}}
	e.observe(t.Context(), OperationalEvent{Kind: OperationalProviderCall})
	e.observe(t.Context(), OperationalEvent{Kind: OperationalWebhookReceipt})
	select {
	case got := <-delivered:
		if got.Kind != OperationalWebhookReceipt {
			t.Fatalf("delivered = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("panic stopped observer worker")
	}
}
