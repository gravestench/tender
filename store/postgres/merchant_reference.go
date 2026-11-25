package postgres

import "github.com/gravestench/tender"

// OperationByMerchantReference finds a bound operation PSP across payments on
// one provider account. The digest narrows the index search; equality confirms
// the full reference to guard against a digest collision.
func (t *transaction) OperationByMerchantReference(scope tender.Scope, provider, account, ref string) (tender.OperationRecord, error) {
	if ref == "" {
		return tender.OperationRecord{}, tender.ErrNotFound
	}
	return read[tender.OperationRecord](t.tx.QueryRowContext(t.ctx, `
		SELECT o.record FROM tender_operations o
		JOIN tender_payments p ON p.merchant_id=o.merchant_id AND p.environment=o.environment AND p.payment_id=o.payment_id
		WHERE o.merchant_id=$1 AND o.environment=$2 AND p.provider=$3 AND p.merchant_account=$4
		AND tender_reference_digest(o.provider_reference)=tender_reference_digest($5) AND o.provider_reference=$5
		LIMIT 1`, scope.MerchantID, scope.Environment, provider, account, ref))
}
