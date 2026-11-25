BEGIN;

ALTER TABLE tender_settlements
    ADD COLUMN batch_id text NOT NULL DEFAULT '',
    ADD COLUMN source_part_id text NOT NULL DEFAULT '',
    ADD COLUMN source_section_id text NOT NULL DEFAULT '',
    ADD COLUMN original_reference text NOT NULL DEFAULT '',
    ADD COLUMN modification_reference text NOT NULL DEFAULT '';

CREATE TABLE tender_reconciliation_inputs (
    merchant_id text NOT NULL,
    environment text NOT NULL,
    provider text NOT NULL,
    merchant_account text NOT NULL,
    run_id text NOT NULL,
    record jsonb NOT NULL,
    PRIMARY KEY (merchant_id,environment,provider,merchant_account,run_id),
    FOREIGN KEY (merchant_id,environment,provider,merchant_account,run_id)
        REFERENCES tender_reconciliation_runs
);

CREATE TABLE tender_reconciliation_payout_history (
    seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id text NOT NULL,
    environment text NOT NULL,
    provider text NOT NULL,
    merchant_account text NOT NULL,
    run_id text NOT NULL,
    payout_id text NOT NULL,
    outcome text NOT NULL,
    evidence jsonb NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (merchant_id,environment,provider,merchant_account,run_id)
        REFERENCES tender_reconciliation_runs
);
CREATE INDEX tender_payout_history_by_run ON tender_reconciliation_payout_history
    (merchant_id,environment,provider,merchant_account,run_id,payout_id,seq);

COMMIT;
