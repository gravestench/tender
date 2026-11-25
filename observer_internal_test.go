package tender

import "testing"

func TestOperationalDiscrepancyCounts(t *testing.T) {
	run := ReconciliationRun{Results: []ReconciliationResult{{Outcome: ReconciliationMatched}, {Outcome: ReconciliationUnmatched}}, PayoutResults: []PayoutReconciliationResult{{Outcome: PayoutMatched}, {Outcome: PayoutMissing}}}
	lines, payouts := operationalDiscrepancyCounts(run)
	if lines != 1 || payouts != 1 {
		t.Fatalf("discrepancies = %d, %d", lines, payouts)
	}
}
