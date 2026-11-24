BEGIN;
ALTER TABLE tender_reconciliation_runs DROP COLUMN input_digest;
COMMIT;
