package main

import (
	"context"
	"time"
)

type checkoutEventProcessor interface {
	Process(context.Context, string) error
}

func (a *app) startCheckoutWorker(wake <-chan struct{}, h checkoutEventProcessor) func() {
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			a.processCheckoutEvents(ctx, h)
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-ticker.C:
			}
		}
	}()
	return func() { stop(); <-done }
}

// The inbox survives dropped wakes and restarts. Close the reader before using
// more connections. Unmatched or temporarily conflicting receipts remain pending.
func (a *app) processCheckoutEvents(ctx context.Context, h checkoutEventProcessor) {
	rows, err := a.ledger.QueryContext(ctx, `SELECT event_id FROM tender_provider_events WHERE merchant_id=$1 AND environment=$2 AND provider=$3 AND merchant_account=$4 AND processed_at IS NULL ORDER BY received_at,event_id`, a.scope.MerchantID, a.scope.Environment, a.provider, a.account)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			rows.Close()
			return
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	for _, id := range ids {
		_ = h.Process(ctx, id)
	}
	for _, o := range orders {
		_ = a.project(ctx, o)
	}
}
