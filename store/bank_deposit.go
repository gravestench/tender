package store

import (
	"time"

	"github.com/gravestench/tender"
)

type bankReviewKey struct {
	scope                                                               tender.Scope
	provider, account, reportID, payoutID, bankAccountToken, movementID string
}
type bankClaimKey struct {
	scope                        tender.Scope
	bankAccountToken, movementID string
}
type bankReferenceKey struct {
	scope                              tender.Scope
	provider, account, payoutReference string
}
type bankClaimOwner struct{ provider, account, reportID, payoutID, bankAccountToken, movementID string }

// Memory cannot read a trusted payout result because it does not implement
// ReconciliationTx. It still models the transactional bank-claim semantics.
func (*memoryTx) PayoutResult(tender.Scope, string, string, string, string) (tender.PayoutReconciliationResult, error) {
	return tender.PayoutReconciliationResult{}, tender.ErrUnsupported
}

func (t *memoryTx) StoreBankDepositResult(scope tender.Scope, provider, account string, r tender.BankDepositResult) (tender.BankDepositResult, error) {
	e := r.Evidence
	k := bankReviewKey{scope, provider, account, r.ReportID, r.PayoutID, e.BankAccountToken, e.MovementID}
	prior, exists := t.bankReviews[k]
	if exists && prior.Evidence != e {
		return tender.BankDepositResult{}, tender.ErrConflict
	}
	owner := bankClaimOwner{provider, account, r.ReportID, r.PayoutID, e.BankAccountToken, e.MovementID}
	for claimKey, prior := range t.bankClaims {
		if claimKey.scope == scope && prior.provider == provider && prior.account == account && prior.reportID == r.ReportID && prior.payoutID == r.PayoutID && prior != owner {
			return tender.BankDepositResult{}, tender.ErrConflict
		}
	}
	if prior, ok := t.bankClaims[bankClaimKey{scope, e.BankAccountToken, e.MovementID}]; ok && prior != owner {
		r.Outcome = tender.BankDepositReused
	}
	if prior, ok := t.bankReferences[bankReferenceKey{scope, provider, account, e.PayoutReference}]; ok && prior != owner {
		r.Outcome = tender.BankDepositReused
	}
	if r.Outcome == tender.BankDepositMatched {
		t.bankClaims[bankClaimKey{scope, e.BankAccountToken, e.MovementID}] = owner
		t.bankReferences[bankReferenceKey{scope, provider, account, e.PayoutReference}] = owner
	}
	if exists && sameBankResult(prior, r) {
		return prior, nil
	}
	t.bankReviews[k] = r
	t.bankHistory = append(t.bankHistory, r)
	return r, nil
}

func sameBankResult(a, b tender.BankDepositResult) bool {
	aAt, bAt := a.Evidence.BookedAt, b.Evidence.BookedAt
	a.Evidence.BookedAt, b.Evidence.BookedAt = time.Time{}, time.Time{}
	return a == b && aAt.Equal(bAt)
}

var _ tender.BankDepositTx = (*memoryTx)(nil)
