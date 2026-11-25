package postgres

import (
	"context"
	"encoding/json"

	"github.com/gravestench/tender"
)

func (t *transaction) EnqueueAdverse(n tender.AdverseNotice) error {
	if err := n.Validate(); err != nil {
		return err
	}
	n.ObservedAt = n.ObservedAt.Round(0).UTC()
	raw, err := json.Marshal(n)
	if err != nil {
		return err
	}
	key := n.Key()
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO tender_adverse_reviews(record) VALUES ($1)
        ON CONFLICT (merchant_id, environment, provider, merchant_account, event_id) DO NOTHING`, string(raw))
	if err != nil {
		return dbError(err)
	}
	var saved []byte
	err = t.tx.QueryRowContext(t.ctx, `SELECT record FROM tender_adverse_reviews
        WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND event_id=$5`,
		key.Scope.MerchantID, key.Scope.Environment, key.Provider, key.MerchantAccount, key.ProviderEventID).Scan(&saved)
	if err != nil {
		return dbError(err)
	}
	var previous tender.AdverseNotice
	if err := json.Unmarshal(saved, &previous); err != nil {
		return err
	}
	if previous != n {
		return tender.ErrConflict
	}
	return nil
}

func (t *transaction) ReviewAdverse(k tender.AdverseKey, d tender.AdverseDecision) error {
	if err := k.Validate(); err != nil {
		return err
	}
	var err error
	d, err = d.Normalize()
	if err != nil {
		return err
	}
	var saved []byte
	err = t.tx.QueryRowContext(t.ctx, `SELECT decision FROM tender_adverse_reviews
        WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND event_id=$5 FOR UPDATE`,
		k.Scope.MerchantID, k.Scope.Environment, k.Provider, k.MerchantAccount, k.ProviderEventID).Scan(&saved)
	if err != nil {
		return dbError(err)
	}
	var processed bool
	err = t.tx.QueryRowContext(t.ctx, `SELECT processed_at IS NOT NULL FROM tender_provider_events
        WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND event_id=$5`,
		k.Scope.MerchantID, k.Scope.Environment, k.Provider, k.MerchantAccount, k.ProviderEventID).Scan(&processed)
	if err != nil {
		return dbError(err)
	}
	if !processed {
		return tender.ErrConflict
	}
	if len(saved) != 0 {
		var previous tender.AdverseDecision
		if err := json.Unmarshal(saved, &previous); err != nil {
			return err
		}
		if previous != d {
			return tender.ErrConflict
		}
		return nil
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO tender_adverse_review_history
        (merchant_id, environment, provider, merchant_account, event_id, decision)
        VALUES ($1,$2,$3,$4,$5,$6)`, k.Scope.MerchantID, k.Scope.Environment, k.Provider, k.MerchantAccount, k.ProviderEventID, string(raw))
	if err != nil {
		return dbError(err)
	}
	_, err = t.tx.ExecContext(t.ctx, `UPDATE tender_adverse_reviews SET decision=$6
        WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND event_id=$5`,
		k.Scope.MerchantID, k.Scope.Environment, k.Provider, k.MerchantAccount, k.ProviderEventID, string(raw))
	return dbError(err)
}

// AdverseReviews returns both open and reviewed items in insertion order.
func (s *Store) AdverseReviews(ctx context.Context, scope tender.Scope) ([]tender.AdverseNotice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.record, a.decision FROM tender_adverse_reviews a
        JOIN tender_provider_events e USING (merchant_id, environment, provider, merchant_account, event_id)
        WHERE a.merchant_id=$1 AND a.environment=$2 AND e.processed_at IS NOT NULL ORDER BY a.seq`, scope.MerchantID, scope.Environment)
	if err != nil {
		return nil, dbError(err)
	}
	defer rows.Close()
	var out []tender.AdverseNotice
	for rows.Next() {
		var raw, decision []byte
		if err := rows.Scan(&raw, &decision); err != nil {
			return nil, dbError(err)
		}
		var n tender.AdverseNotice
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, err
		}
		if len(decision) > 0 {
			var d tender.AdverseDecision
			if err := json.Unmarshal(decision, &d); err != nil {
				return nil, err
			}
			n.Decision = &d
		}
		out = append(out, n)
	}
	return out, dbError(rows.Err())
}

func (s *Store) ReviewAdverse(ctx context.Context, k tender.AdverseKey, d tender.AdverseDecision) error {
	return s.Transact(ctx, func(tx tender.Tx) error { return tx.(tender.AdverseReviewTx).ReviewAdverse(k, d) })
}

// AdverseEvidence yields only rows whose verified inbox receipt has committed
// its processed marker. An unprocessed receipt or direct notice insert cannot
// corroborate a settlement dispute or return.
func (t *transaction) AdverseEvidence(scope tender.Scope, provider, account, paymentID string) ([]tender.AdverseNotice, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT a.record FROM tender_adverse_reviews a
        JOIN tender_provider_events e USING (merchant_id, environment, provider, merchant_account, event_id)
        WHERE a.merchant_id=$1 AND a.environment=$2 AND a.provider=$3 AND a.merchant_account=$4
          AND a.payment_id=$5 AND e.processed_at IS NOT NULL ORDER BY a.seq`,
		scope.MerchantID, scope.Environment, provider, account, paymentID)
	if err != nil {
		return nil, dbError(err)
	}
	defer rows.Close()
	var out []tender.AdverseNotice
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, dbError(err)
		}
		var n tender.AdverseNotice
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, dbError(rows.Err())
}

var _ tender.AdverseReviewTx = (*transaction)(nil)
var _ tender.AdverseEvidenceReader = (*transaction)(nil)
var _ interface {
	AdverseReviews(context.Context, tender.Scope) ([]tender.AdverseNotice, error)
	ReviewAdverse(context.Context, tender.AdverseKey, tender.AdverseDecision) error
} = (*Store)(nil)
