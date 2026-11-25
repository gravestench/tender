package store

import "github.com/gravestench/tender"

// OperationByMerchantReference finds an operation PSP on a provider account,
// including operations belonging to another payment in the same merchant scope.
func (t *memoryTx) OperationByMerchantReference(scope tender.Scope, provider, account, ref string) (tender.OperationRecord, error) {
	if ref == "" {
		return tender.OperationRecord{}, tender.ErrNotFound
	}
	for _, op := range t.operations {
		if op.Scope != scope || op.ProviderReference != ref {
			continue
		}
		p, err := t.Payment(scope, op.PaymentID)
		if err != nil {
			return tender.OperationRecord{}, err
		}
		if p.Provider == provider && p.MerchantAccount == account {
			return op, nil
		}
	}
	return tender.OperationRecord{}, tender.ErrNotFound
}
