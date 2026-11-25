//go:build integration

package contract_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/providers/adyen"
	"github.com/gravestench/tender/store"
)

type achCapabilityStore struct {
	*store.Memory
	wrap func(tender.Tx) tender.Tx
}

func (s achCapabilityStore) Transact(ctx context.Context, fn func(tender.Tx) error) error {
	return s.Memory.Transact(ctx, func(tx tender.Tx) error { return fn(s.wrap(tx)) })
}

type achNoDispatch struct{ calls *int }

func (d achNoDispatch) RoundTrip(*http.Request) (*http.Response, error) {
	*d.calls++
	return nil, errors.New("ACH provider dispatch should have been refused")
}

// A host must not start a debit that it cannot later process into both
// returned funds and a durable, corroborated review notice.
func TestACHRejectsStoreWithoutReturnReview(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(tender.Tx) tender.Tx
	}{
		{"missing adverse writer", func(tx tender.Tx) tender.Tx {
			return struct {
				tender.Tx
				tender.VerifiedEventTx
				tender.VerifiedEvidenceReader
				tender.CaptureFailureTx
				tender.AdverseEvidenceReader
			}{tx, tx.(tender.VerifiedEventTx), tx.(tender.VerifiedEvidenceReader), tx.(tender.CaptureFailureTx), tx.(tender.AdverseEvidenceReader)}
		}},
		{"missing adverse evidence", func(tx tender.Tx) tender.Tx {
			return struct {
				tender.Tx
				tender.VerifiedEventTx
				tender.VerifiedEvidenceReader
				tender.CaptureFailureTx
				tender.AdverseReviewTx
			}{tx, tx.(tender.VerifiedEventTx), tx.(tender.VerifiedEvidenceReader), tx.(tender.CaptureFailureTx), tx.(tender.AdverseReviewTx)}
		}},
		{"missing merchant operation lookup", func(tx tender.Tx) tender.Tx {
			return struct {
				tender.Tx
				tender.VerifiedEventTx
				tender.VerifiedEvidenceReader
				tender.CaptureFailureTx
				tender.AdverseReviewTx
				tender.AdverseEvidenceReader
			}{tx, tx.(tender.VerifiedEventTx), tx.(tender.VerifiedEvidenceReader), tx.(tender.CaptureFailureTx), tx.(tender.AdverseReviewTx), tx.(tender.AdverseEvidenceReader)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			adapter, err := adyen.NewACH(adyen.Config{Environment: tender.EnvironmentMock, BaseURL: "http://127.0.0.1:9/v72", Origin: "http://127.0.0.1", ReturnURL: "http://127.0.0.1/checkout", APIKey: "mock-key", MerchantAccount: "merchant", Transport: achNoDispatch{&calls}})
			if err != nil {
				t.Fatal(err)
			}
			filtered := achCapabilityStore{store.NewMemory(), tc.wrap}
			for _, kind := range []tender.OperationKind{tender.OpCreate, tender.OpConfirm} {
				err := filtered.Transact(t.Context(), func(tx tender.Tx) error { return adapter.CheckStore(kind, tx) })
				if !errors.Is(err, tender.ErrUnsupported) {
					t.Fatalf("ACH %s accepted incomplete store: %v", kind, err)
				}
			}
			engine, err := tender.NewEngine(tender.EngineConfig{Provider: adapter, Store: filtered, Scope: tender.Scope{Environment: tender.EnvironmentMock, MerchantID: "ach-store-check"}, Market: tender.Market{Country: "US", Currency: "USD", Method: tender.MethodACH}, ProviderName: "adyen", MerchantAccount: "merchant"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = engine.Create(t.Context(), tender.Request{PaymentID: "payment", OrderID: "order", Key: "create", Amount: tender.Money{Minor: 1234, Currency: "USD"}})
			if !errors.Is(err, tender.ErrUnsupported) || calls != 0 {
				t.Fatalf("incomplete ACH store dispatched %d calls: %v", calls, err)
			}
		})
	}
}
