package adyen

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/gravestench/tender"
)

// merchantReference includes a lookup ID and a digest of its immutable binding.
// Adyen authenticates the whole reference in a Standard notification. The
// digest prevents an operation ID reused by another host scope or restored
// database from silently binding its provider evidence to a different payment.
func merchantReference(p tender.Payment, op tender.Operation) string {
	raw, _ := json.Marshal([]string{p.Scope.MerchantID, string(p.Scope.Environment), p.MerchantAccount, p.ID, p.AttemptID, op.ID, string(op.Kind), op.ProviderKey})
	sum := sha256.Sum256(raw)
	return op.ID + "." + base64.RawURLEncoding.EncodeToString(sum[:16])
}

// ResolveReference correlates a provider-echoed merchant reference against the
// original payment and reservation in the caller's transaction.
func ResolveReference(tx tender.EventRecords, scope tender.Scope, ref string) (tender.Payment, tender.OperationRecord, error) {
	id, _, ok := strings.Cut(ref, ".")
	if !ok || !reference.MatchString(id) {
		return tender.Payment{}, tender.OperationRecord{}, tender.ErrNotFound
	}
	op, err := tx.Operation(scope, id)
	if err != nil {
		return tender.Payment{}, op, err
	}
	p, err := tx.Payment(scope, op.PaymentID)
	if err != nil {
		return p, op, err
	}
	if ref != merchantReference(p, op.Operation) {
		return p, op, tender.ErrConflict
	}
	return p, op, nil
}
