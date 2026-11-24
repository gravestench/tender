BEGIN;
DROP TABLE tender_reconciliation_history, tender_reconciliation_runs, tender_settlements,
    tender_provider_events, tender_outbox, tender_audit, tender_operations, tender_payments;
COMMIT;
