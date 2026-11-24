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
	if old.SuccessorID != "" && next.SuccessorID != old.SuccessorID ||
		old.ProviderReference != "" && next.ProviderReference != old.ProviderReference {
		return tender.ErrConflict
	}
	// Only the result and its binding may change after reservation.
	old.Status, old.ProviderReference = next.Status, next.ProviderReference
	if next.Operation != old.Operation {
		return tender.ErrConflict
	}
	return nil
}
