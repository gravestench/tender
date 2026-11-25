package tender

import (
	"testing"
	"time"
)

type chargebackMatcherFixture struct {
	Tx
	ReconciliationTx
	payment, otherPayment Payment
	capture, otherCapture OperationRecord
	notices               []AdverseNotice
}

func (f *chargebackMatcherFixture) ResolveSettlementReference(_ Scope, _, _, ref string) (Payment, OperationRecord, error) {
	switch ref {
	case f.payment.ProviderReference:
		return f.payment, OperationRecord{}, nil
	case f.capture.ProviderReference:
		return f.payment, f.capture, nil
	case f.otherCapture.ProviderReference:
		return f.otherPayment, f.otherCapture, nil
	default:
		return Payment{}, OperationRecord{}, ErrNotFound
	}
}

func (f *chargebackMatcherFixture) Payment(_ Scope, id string) (Payment, error) {
	if id == f.payment.ID {
		return f.payment, nil
	}
	return Payment{}, ErrNotFound
}

func (f *chargebackMatcherFixture) OperationByReference(_ Scope, paymentID, ref string) (OperationRecord, error) {
	if paymentID == f.payment.ID && ref == f.capture.ProviderReference {
		return f.capture, nil
	}
	return OperationRecord{}, ErrNotFound
}

func (f *chargebackMatcherFixture) AdverseEvidence(_ Scope, _, _, paymentID string) ([]AdverseNotice, error) {
	if paymentID != f.payment.ID {
		return nil, ErrNotFound
	}
	return f.notices, nil
}

// A chargeback report can name the capture PSP instead of the dispute PSP.
// Neither that row nor the signed dispute alone is sufficient: both must bind
// one fully captured payment before the row can match.
func TestAdyenChargebackCaptureReferenceNeedsSeparateDispute(t *testing.T) {
	scope := Scope{MerchantID: "shop", Environment: EnvironmentMock}
	market := Market{Country: "US", Currency: "USD", Method: MethodVisa}
	now := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
	p := Payment{Scope: scope, ID: "payment", Provider: "adyen", MerchantAccount: "acct", Market: market, AttemptID: "attempt", ProviderReference: "PAYMENTPSP", Amount: Money{Minor: 100, Currency: "USD"}, Snapshot: Snapshot{Status: PaymentCaptured, Funds: Funds{Authorized: 100, Captured: 100}}}
	op := OperationRecord{Operation: Operation{Scope: scope, ID: "capture", PaymentID: p.ID, AttemptID: p.AttemptID, ProviderReference: "CAPTUREPSP", Kind: OpCapture, Status: OperationSucceeded, Amount: p.Amount}}
	f := &chargebackMatcherFixture{payment: p, capture: op}
	e := &Engine{config: EngineConfig{Scope: scope, ProviderName: "adyen", MerchantAccount: "acct", Market: market}}
	line := SettlementLine{ID: "chargeback-row", ProviderReference: p.ProviderReference, OriginalReference: p.ProviderReference, ModificationReference: op.ProviderReference, BatchID: "42", Kind: SettlementReturned, Currency: "USD", GrossMinor: -100, NetMinor: -100, ObservedAt: now}
	without, err := e.matchSettlementLine(f, line)
	if err != nil || without.Outcome != ReconciliationUnconfirmed {
		t.Fatalf("capture reference without dispute matched: %+v, %v", without, err)
	}
	f.notices = []AdverseNotice{{Scope: scope, Provider: "adyen", MerchantAccount: "acct", PaymentID: p.ID, ProviderEventID: "signed-dispute", Kind: "chargeback", ProviderReference: "DISPUTEPSP", OriginalReference: p.ProviderReference, ModificationReference: "DISPUTEPSP", Amount: p.Amount, ObservedAt: now}}
	with, err := e.matchSettlementLine(f, line)
	if err != nil || with.Outcome != ReconciliationMatched || with.ExpectedMinor != 100 || with.AdverseEventID != "signed-dispute" {
		t.Fatalf("signed dispute and capture reference did not match: %+v, %v", with, err)
	}
	other := p
	other.ID, other.AttemptID, other.ProviderReference = "other-payment", "other-attempt", "OTHERPAYMENTPSP"
	f.otherPayment = other
	f.otherCapture = OperationRecord{Operation: Operation{Scope: scope, ID: "other-capture", PaymentID: other.ID, AttemptID: other.AttemptID, ProviderReference: "OTHERCAPTUREPSP", Kind: OpCapture, Status: OperationSucceeded, Amount: other.Amount}}
	crossPayment := line
	crossPayment.ModificationReference = f.otherCapture.ProviderReference
	if got, err := e.matchSettlementLine(f, crossPayment); err != nil || got.Outcome != ReconciliationUnconfirmed {
		t.Fatalf("another payment's capture backed a dispute: %+v, %v", got, err)
	}
	exact := line
	exact.ID, exact.ModificationReference = "same-dispute-second-row", "DISPUTEPSP"
	results, err := e.matchSettlementLines(f, f, []SettlementLine{line, exact})
	if err != nil || len(results) != 2 || results[0].Outcome != ReconciliationUnconfirmed || results[1].Outcome != ReconciliationUnconfirmed {
		t.Fatalf("one signed dispute counted twice in report: %+v, %v", results, err)
	}
}
