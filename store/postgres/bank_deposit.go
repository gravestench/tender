package postgres

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/gravestench/tender"
)

func (t *transaction) PayoutResult(scope tender.Scope, provider, account, reportID, payoutID string) (tender.PayoutReconciliationResult, error) {
	return read[tender.PayoutReconciliationResult](t.tx.QueryRowContext(t.ctx, `SELECT h.evidence
   FROM tender_reconciliation_payout_history h
   JOIN tender_reconciliation_runs r ON r.merchant_id=h.merchant_id AND r.environment=h.environment AND r.provider=h.provider AND r.merchant_account=h.merchant_account AND r.run_id=h.run_id
   WHERE h.merchant_id=$1 AND h.environment=$2 AND h.provider=$3 AND h.merchant_account=$4 AND h.run_id=$5 AND h.payout_id=$6 AND r.finished_at IS NOT NULL
   ORDER BY h.seq DESC LIMIT 1`, scope.MerchantID, scope.Environment, provider, account, reportID, payoutID))
}

func (t *transaction) StoreBankDepositResult(scope tender.Scope, provider, account string, r tender.BankDepositResult) (tender.BankDepositResult, error) {
	e := r.Evidence
	prior, err := read[tender.BankDepositResult](t.tx.QueryRowContext(t.ctx, `SELECT evidence FROM tender_bank_deposit_reviews
   WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND report_id=$5 AND payout_id=$6 AND bank_account_token=$7 AND movement_id=$8`,
		scope.MerchantID, scope.Environment, provider, account, r.ReportID, r.PayoutID, e.BankAccountToken, e.MovementID))
	priorExists := err == nil
	if priorExists && !sameBankEvidence(prior.Evidence, e) {
		return tender.BankDepositResult{}, tender.ErrConflict
	}
	if err != nil && !errors.Is(err, tender.ErrNotFound) {
		return tender.BankDepositResult{}, err
	}
	rows, err := t.tx.QueryContext(t.ctx, `SELECT provider,merchant_account,report_id,payout_id,bank_account_token,movement_id
  FROM tender_bank_deposit_claims WHERE merchant_id=$1 AND environment=$2 AND
  ((bank_account_token=$3 AND movement_id=$4) OR
   (provider=$5 AND merchant_account=$6 AND payout_reference=$7) OR
   (provider=$5 AND merchant_account=$6 AND report_id=$8 AND payout_id=$9))`,
		scope.MerchantID, scope.Environment, e.BankAccountToken, e.MovementID, provider, account, e.PayoutReference, r.ReportID, r.PayoutID)
	if err != nil {
		return tender.BankDepositResult{}, dbError(err)
	}
	claimedBySelf := false
	for rows.Next() {
		var claimedProvider, claimedAccount, claimedReport, claimedPayout, token, movement string
		if err := rows.Scan(&claimedProvider, &claimedAccount, &claimedReport, &claimedPayout, &token, &movement); err != nil {
			rows.Close()
			return tender.BankDepositResult{}, dbError(err)
		}
		if claimedProvider == provider && claimedAccount == account && claimedReport == r.ReportID && claimedPayout == r.PayoutID {
			if token != e.BankAccountToken || movement != e.MovementID {
				rows.Close()
				return tender.BankDepositResult{}, tender.ErrConflict
			}
			claimedBySelf = true
			continue
		}
		r.Outcome = tender.BankDepositReused
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return tender.BankDepositResult{}, dbError(err)
	}
	rows.Close()
	if priorExists && sameBankResult(prior, r) {
		return prior, nil
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return tender.BankDepositResult{}, err
	}
	if priorExists {
		_, err = t.tx.ExecContext(t.ctx, `UPDATE tender_bank_deposit_reviews SET outcome=$9,evidence=$10,recorded_at=CURRENT_TIMESTAMP
    WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND report_id=$5 AND payout_id=$6 AND bank_account_token=$7 AND movement_id=$8`,
			scope.MerchantID, scope.Environment, provider, account, r.ReportID, r.PayoutID, e.BankAccountToken, e.MovementID, r.Outcome, string(raw))
	} else {
		_, err = t.tx.ExecContext(t.ctx, `INSERT INTO tender_bank_deposit_reviews
  (merchant_id,environment,provider,merchant_account,report_id,payout_id,bank_account_token,movement_id,outcome,evidence)
  VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			scope.MerchantID, scope.Environment, provider, account, r.ReportID, r.PayoutID, e.BankAccountToken, e.MovementID, r.Outcome, string(raw))
	}
	if err != nil {
		return tender.BankDepositResult{}, dbError(err)
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO tender_bank_deposit_review_history
  (merchant_id,environment,provider,merchant_account,report_id,payout_id,bank_account_token,movement_id,outcome,evidence)
  VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		scope.MerchantID, scope.Environment, provider, account, r.ReportID, r.PayoutID, e.BankAccountToken, e.MovementID, r.Outcome, string(raw))
	if err != nil {
		return tender.BankDepositResult{}, dbError(err)
	}
	if r.Outcome == tender.BankDepositMatched && !claimedBySelf {
		_, err = t.tx.ExecContext(t.ctx, `INSERT INTO tender_bank_deposit_claims
   (merchant_id,environment,bank_account_token,movement_id,provider,merchant_account,report_id,payout_id,payout_reference)
   VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			scope.MerchantID, scope.Environment, e.BankAccountToken, e.MovementID, provider, account, r.ReportID, r.PayoutID, e.PayoutReference)
		if err != nil {
			return tender.BankDepositResult{}, dbError(err)
		}
	}
	return r, nil
}

func sameBankResult(a, b tender.BankDepositResult) bool {
	return a.ReportID == b.ReportID && a.PayoutID == b.PayoutID && a.Outcome == b.Outcome && sameBankEvidence(a.Evidence, b.Evidence)
}

func sameBankEvidence(a, b tender.BankDepositEvidence) bool {
	aAt, bAt := a.BookedAt, b.BookedAt
	a.BookedAt, b.BookedAt = time.Time{}, time.Time{}
	return a == b && aAt.Equal(bAt)
}

var _ tender.BankDepositTx = (*transaction)(nil)
