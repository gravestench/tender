BEGIN;

CREATE TABLE tender_bank_deposit_reviews (
    merchant_id text NOT NULL,
    environment text NOT NULL,
    provider text NOT NULL,
    merchant_account text NOT NULL,
    report_id text NOT NULL,
    payout_id text NOT NULL,
    bank_account_token text NOT NULL,
    movement_id text NOT NULL,
    outcome text NOT NULL,
    evidence jsonb NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (merchant_id, environment, provider, merchant_account, report_id, payout_id, bank_account_token, movement_id),
    FOREIGN KEY (merchant_id, environment, provider, merchant_account, report_id)
        REFERENCES tender_reconciliation_runs(merchant_id, environment, provider, merchant_account, run_id)
);

-- Keep each bank verdict transition even when the operational review changes.
CREATE TABLE tender_bank_deposit_review_history (
    seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id text NOT NULL,
    environment text NOT NULL,
    provider text NOT NULL,
    merchant_account text NOT NULL,
    report_id text NOT NULL,
    payout_id text NOT NULL,
    bank_account_token text NOT NULL,
    movement_id text NOT NULL,
    outcome text NOT NULL,
    evidence jsonb NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (merchant_id, environment, provider, merchant_account, report_id, payout_id, bank_account_token, movement_id)
        REFERENCES tender_bank_deposit_reviews(merchant_id, environment, provider, merchant_account, report_id, payout_id, bank_account_token, movement_id)
);
CREATE INDEX tender_bank_review_history_by_payout ON tender_bank_deposit_review_history
    (merchant_id, environment, provider, merchant_account, report_id, payout_id, seq);

CREATE TABLE tender_bank_deposit_claims (
    merchant_id text NOT NULL,
    environment text NOT NULL,
    bank_account_token text NOT NULL,
    movement_id text NOT NULL,
    provider text NOT NULL,
    merchant_account text NOT NULL,
    report_id text NOT NULL,
    payout_id text NOT NULL,
    payout_reference text NOT NULL,
    PRIMARY KEY (merchant_id, environment, bank_account_token, movement_id),
    UNIQUE (merchant_id, environment, provider, merchant_account, payout_reference),
    UNIQUE (merchant_id, environment, provider, merchant_account, report_id, payout_id),
    FOREIGN KEY (merchant_id, environment, provider, merchant_account, report_id, payout_id, bank_account_token, movement_id)
        REFERENCES tender_bank_deposit_reviews(merchant_id, environment, provider, merchant_account, report_id, payout_id, bank_account_token, movement_id)
);

COMMIT;
