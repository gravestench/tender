package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gravestench/tender"
)

var _ tender.ReconciliationTx = (*transaction)(nil)
var _ tender.ReturnEvidenceClaimTx = (*transaction)(nil)

func (t *transaction) ClaimReturnEvidenceSet(scope tender.Scope, provider, account, reportID string, claims []tender.ReturnEvidenceClaim) (bool, error) {
	if scope.Validate() != nil || provider == "" || account == "" || reportID == "" || len(claims) == 0 {
		return false, tender.ErrValidation
	}
	// A transaction-level lock makes the check and insert an atomic set even
	// when two different report IDs race to claim one signed receipt.
	if _, err := t.tx.ExecContext(t.ctx, `LOCK TABLE tender_reconciliation_return_claims IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return false, dbError(err)
	}
	seen := make(map[string]bool, len(claims))
	lines := make(map[string]bool, len(claims))
	for _, claim := range claims {
		if strings.TrimSpace(claim.ProviderEventID) == "" || len(claim.ProviderEventID) > 200 || strings.TrimSpace(claim.LineID) == "" || len(claim.LineID) > 200 || seen[claim.ProviderEventID] || lines[claim.LineID] {
			return false, tender.ErrValidation
		}
		seen[claim.ProviderEventID] = true
		lines[claim.LineID] = true
		var processed bool
		err := t.tx.QueryRowContext(t.ctx, `SELECT EXISTS (
			SELECT 1 FROM tender_adverse_reviews a JOIN tender_provider_events e
			USING (merchant_id, environment, provider, merchant_account, event_id)
			WHERE a.merchant_id=$1 AND a.environment=$2 AND a.provider=$3 AND a.merchant_account=$4
			AND a.event_id=$5 AND e.processed_at IS NOT NULL)`, scope.MerchantID, scope.Environment, provider, account, claim.ProviderEventID).Scan(&processed)
		if err != nil {
			return false, dbError(err)
		}
		if !processed {
			return false, tender.ErrConflict
		}
		var priorReport, priorLine string
		err = t.tx.QueryRowContext(t.ctx, `SELECT report_id,line_id FROM tender_reconciliation_return_claims
			WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND event_id=$5`,
			scope.MerchantID, scope.Environment, provider, account, claim.ProviderEventID).Scan(&priorReport, &priorLine)
		if err == nil && (priorReport != reportID || priorLine != claim.LineID) {
			return false, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, dbError(err)
		}
		var priorEvent string
		err = t.tx.QueryRowContext(t.ctx, `SELECT event_id FROM tender_reconciliation_return_claims
			WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND report_id=$5 AND line_id=$6`,
			scope.MerchantID, scope.Environment, provider, account, reportID, claim.LineID).Scan(&priorEvent)
		if err == nil && priorEvent != claim.ProviderEventID {
			return false, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, dbError(err)
		}
	}
	for _, claim := range claims {
		_, err := t.tx.ExecContext(t.ctx, `INSERT INTO tender_reconciliation_return_claims
			(merchant_id,environment,provider,merchant_account,event_id,report_id,line_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (merchant_id,environment,provider,merchant_account,event_id) DO NOTHING`,
			scope.MerchantID, scope.Environment, provider, account, claim.ProviderEventID, reportID, claim.LineID)
		if err != nil {
			return false, dbError(err)
		}
	}
	return true, nil
}

func (t *transaction) ReconciliationRun(scope tender.Scope, provider, account, id string) (tender.ReconciliationRun, error) {
	var digest string
	err := t.tx.QueryRowContext(t.ctx, `SELECT input_digest FROM tender_reconciliation_runs WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND run_id=$5`, scope.MerchantID, scope.Environment, provider, account, id).Scan(&digest)
	if err != nil {
		return tender.ReconciliationRun{}, dbError(err)
	}
	return tender.ReconciliationRun{ID: id, Digest: digest}, nil
}

func (t *transaction) InsertReconciliationRun(scope tender.Scope, provider, account string, run tender.ReconciliationRun) error {
	_, err := t.tx.ExecContext(t.ctx, `INSERT INTO tender_reconciliation_runs (merchant_id,environment,provider,merchant_account,run_id,report_id,input_digest) VALUES ($1,$2,$3,$4,$5,$5,$6)`, scope.MerchantID, scope.Environment, provider, account, run.ID, run.Digest)
	return dbError(err)
}

func (t *transaction) InsertSettlementLine(scope tender.Scope, provider, account, reportID string, l tender.SettlementLine) error {
	_, err := t.tx.ExecContext(t.ctx, `INSERT INTO tender_settlements (merchant_id,environment,provider,merchant_account,report_id,line_id,provider_reference,entry_kind,currency,gross_minor,fee_minor,net_minor,payout_reference,observed_at,batch_id,source_part_id,source_section_id,original_reference,modification_reference) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`, scope.MerchantID, scope.Environment, provider, account, reportID, l.ID, l.ProviderReference, l.Kind, l.Currency, l.GrossMinor, l.FeeMinor, l.NetMinor, l.PayoutReference, l.ObservedAt, l.BatchID, l.SourcePartID, l.SourceSectionID, l.OriginalReference, l.ModificationReference)
	return dbError(err)
}

func (t *transaction) InsertReconciliationPayoutInput(scope tender.Scope, provider, account, reportID string, report tender.SettlementReport) error {
	input := struct {
		Payouts     []tender.PayoutLine
		Adjustments []tender.PayoutAdjustment
		Parts       []tender.ReportPart
		Sections    []tender.ReportSection
	}{report.Payouts, report.Adjustments, report.Parts, report.Sections}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO tender_reconciliation_inputs(merchant_id,environment,provider,merchant_account,run_id,record) VALUES ($1,$2,$3,$4,$5,$6)`, scope.MerchantID, scope.Environment, provider, account, reportID, string(raw))
	return dbError(err)
}

func (t *transaction) AppendPayoutReconciliationResult(scope tender.Scope, provider, account, runID string, r tender.PayoutReconciliationResult) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO tender_reconciliation_payout_history(merchant_id,environment,provider,merchant_account,run_id,payout_id,outcome,evidence) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, scope.MerchantID, scope.Environment, provider, account, runID, r.PayoutID, r.Outcome, string(raw))
	if err != nil || r.Outcome == tender.PayoutMatched {
		return dbError(err)
	}
	// A later provider discrepancy invalidates an operational bank match in
	// this same transaction. Keep the original claim and append the verdict.
	_, err = t.tx.ExecContext(t.ctx, `WITH demoted AS (
	  UPDATE tender_bank_deposit_reviews
	  SET outcome=$7,evidence=jsonb_set(evidence,'{Outcome}',to_jsonb($7::text)),recorded_at=CURRENT_TIMESTAMP
	  WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND report_id=$5 AND payout_id=$6 AND outcome='matched'
	  RETURNING merchant_id,environment,provider,merchant_account,report_id,payout_id,bank_account_token,movement_id,outcome,evidence,recorded_at
	)
	INSERT INTO tender_bank_deposit_review_history
	  (merchant_id,environment,provider,merchant_account,report_id,payout_id,bank_account_token,movement_id,outcome,evidence,recorded_at)
	SELECT merchant_id,environment,provider,merchant_account,report_id,payout_id,bank_account_token,movement_id,outcome,evidence,recorded_at FROM demoted`,
		scope.MerchantID, scope.Environment, provider, account, runID, r.PayoutID, tender.BankDepositProviderUnconfirmed)
	return dbError(err)
}

func (t *transaction) PayoutReconciliationResults(scope tender.Scope, provider, account, runID string) ([]tender.PayoutReconciliationResult, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT evidence FROM tender_reconciliation_payout_history WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND run_id=$5 ORDER BY seq`, scope.MerchantID, scope.Environment, provider, account, runID)
	if err != nil {
		return nil, dbError(err)
	}
	defer rows.Close()
	var results []tender.PayoutReconciliationResult
	for rows.Next() {
		r, err := read[tender.PayoutReconciliationResult](rows)
		if err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, dbError(rows.Err())
}

func (t *transaction) ResolveSettlementReference(scope tender.Scope, provider, account, ref string) (tender.Payment, tender.OperationRecord, error) {
	// Payment PSPs are unique on the route; operation PSPs are unique only
	// within one payment. Klarna can use the same order PSP in both namespaces
	// for that payment. Collapse that pair to the payment-level observation,
	// while refusing a reference shared by different payments.
	const query = `SELECT p.record,NULL::jsonb FROM tender_payments p
		WHERE p.merchant_id=$1 AND p.environment=$2 AND p.provider=$3 AND p.merchant_account=$4
		AND tender_reference_digest(p.provider_reference)=tender_reference_digest($5) AND p.provider_reference=$5
		UNION ALL
		SELECT p.record,o.record FROM tender_operations o JOIN tender_payments p
		ON p.merchant_id=o.merchant_id AND p.environment=o.environment AND p.payment_id=o.payment_id
		WHERE o.merchant_id=$1 AND o.environment=$2 AND p.provider=$3 AND p.merchant_account=$4
		AND tender_reference_digest(o.provider_reference)=tender_reference_digest($5) AND o.provider_reference=$5
		LIMIT 3`
	rows, err := t.tx.QueryContext(t.ctx, query, scope.MerchantID, scope.Environment, provider, account, ref)
	if err != nil {
		return tender.Payment{}, tender.OperationRecord{}, dbError(err)
	}
	defer rows.Close()
	var p tender.Payment
	var op tender.OperationRecord
	var paymentMatch bool
	var count int
	for rows.Next() {
		count++
		if count > 2 {
			return tender.Payment{}, tender.OperationRecord{}, tender.ErrConflict
		}
		var paymentRaw, operationRaw []byte
		if err := rows.Scan(&paymentRaw, &operationRaw); err != nil {
			return tender.Payment{}, tender.OperationRecord{}, dbError(err)
		}
		var matched tender.Payment
		if err := json.Unmarshal(paymentRaw, &matched); err != nil {
			return tender.Payment{}, tender.OperationRecord{}, err
		}
		if count > 1 && matched.ID != p.ID {
			return tender.Payment{}, tender.OperationRecord{}, tender.ErrConflict
		}
		p = matched
		if operationRaw == nil {
			paymentMatch = true
		} else {
			if err := json.Unmarshal(operationRaw, &op); err != nil {
				return tender.Payment{}, tender.OperationRecord{}, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return tender.Payment{}, tender.OperationRecord{}, dbError(err)
	}
	if count == 0 {
		return tender.Payment{}, tender.OperationRecord{}, tender.ErrNotFound
	}
	if paymentMatch {
		op = tender.OperationRecord{}
	}
	return p, op, nil
}

func (t *transaction) AppendReconciliationResult(scope tender.Scope, provider, account, runID string, r tender.ReconciliationResult) error {
	evidence, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var paymentID any
	if r.PaymentID != "" {
		paymentID = r.PaymentID
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO tender_reconciliation_history (merchant_id,environment,provider,merchant_account,run_id,line_id,payment_id,outcome,evidence) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, scope.MerchantID, scope.Environment, provider, account, runID, r.LineID, paymentID, r.Outcome, string(evidence))
	return dbError(err)
}

func (t *transaction) FinishReconciliationRun(scope tender.Scope, provider, account, runID string) error {
	_, err := t.tx.ExecContext(t.ctx, `UPDATE tender_reconciliation_runs SET finished_at=CURRENT_TIMESTAMP WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND run_id=$5 AND finished_at IS NULL`, scope.MerchantID, scope.Environment, provider, account, runID)
	return dbError(err)
}

func (t *transaction) ReconciliationResults(scope tender.Scope, provider, account, runID string) ([]tender.ReconciliationResult, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT evidence FROM tender_reconciliation_history WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND run_id=$5 AND outcome <> 'report_conflict' ORDER BY seq`, scope.MerchantID, scope.Environment, provider, account, runID)
	if err != nil {
		return nil, dbError(err)
	}
	defer rows.Close()
	var results []tender.ReconciliationResult
	for rows.Next() {
		r, err := read[tender.ReconciliationResult](rows)
		if err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, dbError(rows.Err())
}

// ReconciliationHistory provides persisted results and conflict attempts for
// a host review screen. Scope, route and report identity always constrain it.
func (s *Store) ReconciliationHistory(ctx context.Context, scope tender.Scope, provider, account, reportID string) ([]tender.ReconciliationResult, error) {
	return list[tender.ReconciliationResult](ctx, s.db, `SELECT evidence FROM tender_reconciliation_history WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND run_id=$5 ORDER BY seq`, scope.MerchantID, scope.Environment, provider, account, reportID)
}

// OpenReconciliationDiscrepancies lists the latest unresolved result per report
// line, including conflicting report identities, for one scoped route.
func (s *Store) OpenReconciliationDiscrepancies(ctx context.Context, scope tender.Scope, provider, account string) ([]tender.ReconciliationDiscrepancy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id,evidence FROM (
		SELECT DISTINCT ON (run_id,line_id) run_id,line_id,outcome,evidence,seq
		FROM tender_reconciliation_history
		WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4
		ORDER BY run_id,line_id,seq DESC
	) latest WHERE outcome <> 'matched' ORDER BY seq`, scope.MerchantID, scope.Environment, provider, account)
	if err != nil {
		return nil, dbError(err)
	}
	defer rows.Close()
	var items []tender.ReconciliationDiscrepancy
	for rows.Next() {
		var item tender.ReconciliationDiscrepancy
		var raw []byte
		if err := rows.Scan(&item.ReportID, &raw); err != nil {
			return nil, dbError(err)
		}
		if err := json.Unmarshal(raw, &item.Result); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, dbError(rows.Err())
}

// OpenPayoutDiscrepancies returns the latest unresolved payout result per
// report and payout ID for one merchant and processor route.
func (s *Store) OpenPayoutDiscrepancies(ctx context.Context, scope tender.Scope, provider, account string) ([]tender.PayoutReconciliationDiscrepancy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id,evidence FROM (
		SELECT DISTINCT ON (run_id,payout_id) run_id,payout_id,outcome,evidence,seq
		FROM tender_reconciliation_payout_history
		WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4
		ORDER BY run_id,payout_id,seq DESC
	) latest WHERE outcome <> 'matched' ORDER BY seq`, scope.MerchantID, scope.Environment, provider, account)
	if err != nil {
		return nil, dbError(err)
	}
	defer rows.Close()
	var items []tender.PayoutReconciliationDiscrepancy
	for rows.Next() {
		var item tender.PayoutReconciliationDiscrepancy
		var raw []byte
		if err := rows.Scan(&item.ReportID, &raw); err != nil {
			return nil, dbError(err)
		}
		if err := json.Unmarshal(raw, &item.Result); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, dbError(rows.Err())
}
