//go:build integration

package contract_test

import (
	"errors"
	"testing"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/providers/adyen"
	"github.com/gravestench/tender/store"
)

// A card route that cannot distinguish a dispute PSP from another payment's
// operation must refuse dispatch before it can collect funds.
func TestAdyenCardRejectsStoreWithoutMerchantOperationLookup(t *testing.T) {
	calls := 0
	adapter, err := adyen.New(adyen.Config{Environment: tender.EnvironmentMock, BaseURL: "http://127.0.0.1:9/v72", Origin: "http://127.0.0.1", ReturnURL: "http://127.0.0.1/checkout", APIKey: "mock-key", MerchantAccount: "merchant", Transport: achNoDispatch{&calls}})
	if err != nil {
		t.Fatal(err)
	}
	filtered := achCapabilityStore{store.NewMemory(), func(tx tender.Tx) tender.Tx {
		return struct {
			tender.Tx
			tender.VerifiedEventTx
			tender.VerifiedEvidenceReader
			tender.CaptureFailureTx
			tender.AdverseReviewTx
			tender.AdverseEvidenceReader
		}{tx, tx.(tender.VerifiedEventTx), tx.(tender.VerifiedEvidenceReader), tx.(tender.CaptureFailureTx), tx.(tender.AdverseReviewTx), tx.(tender.AdverseEvidenceReader)}
	}}
	for _, kind := range []tender.OperationKind{tender.OpCreate, tender.OpConfirm, tender.OpCapture} {
		err := filtered.Transact(t.Context(), func(tx tender.Tx) error { return adapter.CheckStore(kind, tx) })
		if !errors.Is(err, tender.ErrUnsupported) {
			t.Fatalf("Adyen card %s accepted incomplete store: %v", kind, err)
		}
	}
	engine, err := tender.NewEngine(tender.EngineConfig{Provider: adapter, Store: filtered, Scope: tender.Scope{Environment: tender.EnvironmentMock, MerchantID: "card-store-check"}, Market: tender.Market{Country: "US", Currency: "USD", Method: tender.MethodVisa}, ProviderName: "adyen", MerchantAccount: "merchant"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.Create(t.Context(), tender.Request{PaymentID: "payment", OrderID: "order", Key: "create", Amount: tender.Money{Minor: 1234, Currency: "USD"}})
	if !errors.Is(err, tender.ErrUnsupported) || calls != 0 {
		t.Fatalf("incomplete Adyen card store dispatched %d calls: %v", calls, err)
	}
}
