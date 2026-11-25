package tender_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/store"
)

func TestOperationalObserverCommittedReportConflict(t *testing.T) {
	events := make(chan tender.OperationalEvent, 1)
	e := observedEngine(t, reconciliationStore{Store: store.NewMemory(), conflict: true}, &processor{execute: authorized}, observerFunc(func(_ context.Context, event tender.OperationalEvent) error { events <- event; return nil }))
	report := tender.SettlementReport{ID: "sensitive-report-id", Feed: tender.FeedHostVerified, Lines: []tender.SettlementLine{{ID: "row", ProviderReference: "unmatched", Currency: "USD", Kind: tender.SettlementCaptured, GrossMinor: 100, NetMinor: 100, ObservedAt: time.Now().UTC()}}}
	_, err := e.Reconcile(t.Context(), report)
	if !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("reconcile = %v", err)
	}
	got := waitOperationalEvent(t, events)
	if got.Kind != tender.OperationalReconciliation || got.ErrorKind != tender.ErrConflict || got.Discrepancies != 1 {
		t.Fatalf("conflict event = %+v", got)
	}
	if b, _ := json.Marshal(got); strings.Contains(string(b), "sensitive-report-id") {
		t.Fatalf("report ID leaked: %s", b)
	}
}
