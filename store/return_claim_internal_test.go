package store

import (
	"testing"

	"github.com/gravestench/tender"
)

func TestMemoryReturnClaimAtomicSet(t *testing.T) {
	m := NewMemory()
	scope := tender.Scope{MerchantID: "shop", Environment: tender.EnvironmentMock}
	for _, id := range []string{"event-a", "event-b"} {
		m.state.events[tender.VerifiedEventKey{Scope: scope, Provider: "adyen", MerchantAccount: "acct", ID: id}] = inboxEvent{processed: true}
		m.state.adverse[tender.AdverseKey{Scope: scope, Provider: "adyen", MerchantAccount: "acct", ProviderEventID: id}] = tender.AdverseNotice{}
	}
	claim := func(report string, claims ...tender.ReturnEvidenceClaim) (bool, error) {
		var accepted bool
		err := m.Transact(t.Context(), func(tx tender.Tx) error {
			var err error
			accepted, err = tx.(tender.ReturnEvidenceClaimTx).ClaimReturnEvidenceSet(scope, "adyen", "acct", report, claims)
			return err
		})
		return accepted, err
	}
	first := tender.ReturnEvidenceClaim{ProviderEventID: "event-a", LineID: "line-a"}
	second := tender.ReturnEvidenceClaim{ProviderEventID: "event-b", LineID: "line-b"}
	if ok, err := claim("report-a", first); err != nil || !ok {
		t.Fatalf("first claim: %t, %v", ok, err)
	}
	if ok, err := claim("report-a", first); err != nil || !ok {
		t.Fatalf("same report/line replay: %t, %v", ok, err)
	}
	if ok, err := claim("report-b", second, first); err != nil || ok {
		t.Fatalf("conflicting set accepted: %t, %v", ok, err)
	}
	if ok, err := claim("report-c", second); err != nil || !ok {
		t.Fatalf("partial claim survived rejected set: %t, %v", ok, err)
	}
	if ok, err := claim("report-b", first); err != nil || ok {
		t.Fatalf("another report spent same event: %t, %v", ok, err)
	}
}
