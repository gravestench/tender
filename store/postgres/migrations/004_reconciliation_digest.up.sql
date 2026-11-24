BEGIN;
ALTER TABLE tender_reconciliation_runs ADD COLUMN input_digest text NOT NULL DEFAULT '';
COMMIT;
