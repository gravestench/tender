// Package binding checks durable identities shared by Tender's stores.
package binding

import "github.com/gravestench/tender"

func PaymentUpdate(old, next tender.Payment) error {
	if next.OrderID != old.OrderID || next.Amount != old.Amount || next.Version <= old.Version || next.Version != old.Version+1 ||
		next.AttemptID != old.AttemptID || next.Market != old.Market ||
		next.Provider != old.Provider || next.MerchantAccount != old.MerchantAccount ||
		old.ProviderReference != "" && old.ProviderReference != next.ProviderReference {
		return tender.ErrConflict
	}
	return nil
}

func OperationUpdate(old, next tender.OperationRecord) error {
	if !old.AwaitingEvidence && next.AwaitingEvidence ||
		old.AwaitingEvidence && !next.AwaitingEvidence && next.Status == tender.OperationReserved && next.ProviderEvidenceDigest == ([32]byte{}) ||
		old.SuccessorID != "" && next.SuccessorID != old.SuccessorID ||
		old.ProviderReference != "" && next.ProviderReference != old.ProviderReference {
		return tender.ErrConflict
	}
	// Only the original reserved operation may add its pre-dispatch evidence.
	if old.AwaitingEvidence && !next.AwaitingEvidence && old.ProviderEvidenceDigest == ([32]byte{}) && old.Status == tender.OperationReserved && next.Status == tender.OperationReserved {
		old.ProviderEvidenceDigest = next.ProviderEvidenceDigest
	}
	// Only the result and its binding may change after reservation.
	old.Status, old.ProviderReference = next.Status, next.ProviderReference
	if next.Operation != old.Operation {
		return tender.ErrConflict
	}
	return nil
}
