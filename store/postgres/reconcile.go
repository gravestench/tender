package postgres

import (
	"context"
	"encoding/json"

	"github.com/gravestench/tender"
)

var _ tender.ReconciliationTx = (*transaction)(nil)

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
	_, err := t.tx.ExecContext(t.ctx, `INSERT INTO tender_settlements (merchant_id,environment,provider,merchant_account,report_id,line_id,provider_reference,entry_kind,currency,gross_minor,fee_minor,net_minor,payout_reference,observed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, scope.MerchantID, scope.Environment, provider, account, reportID, l.ID, l.ProviderReference, l.Kind, l.Currency, l.GrossMinor, l.FeeMinor, l.NetMinor, l.PayoutReference, l.ObservedAt)
	return dbError(err)
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
