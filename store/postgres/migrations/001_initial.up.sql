BEGIN;

-- Generated identity columns index the exact values in the persisted record.
-- One payment currently owns one immutable attempt and processor route.
CREATE TABLE tender_payments (
    record jsonb NOT NULL,
    merchant_id text GENERATED ALWAYS AS (record #>> '{Scope,MerchantID}') STORED NOT NULL,
    environment text GENERATED ALWAYS AS (record #>> '{Scope,Environment}') STORED NOT NULL,
    payment_id text GENERATED ALWAYS AS (record->>'ID') STORED NOT NULL,
    order_id text GENERATED ALWAYS AS (record->>'OrderID') STORED NOT NULL,
    attempt_id text GENERATED ALWAYS AS (record->>'AttemptID') STORED NOT NULL,
    provider text GENERATED ALWAYS AS (record->>'Provider') STORED NOT NULL,
    merchant_account text GENERATED ALWAYS AS (record->>'MerchantAccount') STORED NOT NULL,
    provider_reference text GENERATED ALWAYS AS (NULLIF(record->>'ProviderReference', '')) STORED,
    PRIMARY KEY (merchant_id, environment, payment_id),
    UNIQUE (merchant_id, environment, order_id),
    UNIQUE (merchant_id, environment, payment_id, attempt_id),
    UNIQUE (merchant_id, environment, provider, merchant_account, provider_reference)
);
CREATE TABLE tender_operations (
    record jsonb NOT NULL,
    merchant_id text GENERATED ALWAYS AS (record #>> '{Scope,MerchantID}') STORED NOT NULL,
    environment text GENERATED ALWAYS AS (record #>> '{Scope,Environment}') STORED NOT NULL,
    operation_id text GENERATED ALWAYS AS (record->>'ID') STORED NOT NULL,
    payment_id text GENERATED ALWAYS AS (record->>'PaymentID') STORED NOT NULL,
    attempt_id text GENERATED ALWAYS AS (record->>'AttemptID') STORED NOT NULL,
    operation_key text GENERATED ALWAYS AS (record->>'Key') STORED NOT NULL,
    status text GENERATED ALWAYS AS (record->>'Status') STORED NOT NULL,
    provider_reference text GENERATED ALWAYS AS (NULLIF(record->>'ProviderReference', '')) STORED,
    PRIMARY KEY (merchant_id, environment, operation_id),
    UNIQUE (merchant_id, environment, operation_key),
    UNIQUE (merchant_id, environment, payment_id, provider_reference),
    FOREIGN KEY (merchant_id, environment, payment_id, attempt_id)
        REFERENCES tender_payments (merchant_id, environment, payment_id, attempt_id)
);
CREATE UNIQUE INDEX tender_one_active_operation ON tender_operations (merchant_id, environment, payment_id)
    WHERE status NOT IN ('succeeded', 'failed');

CREATE TABLE tender_audit (
    seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    record jsonb NOT NULL,
    merchant_id text GENERATED ALWAYS AS (record #>> '{Scope,MerchantID}') STORED NOT NULL,
    environment text GENERATED ALWAYS AS (record #>> '{Scope,Environment}') STORED NOT NULL,
    payment_id text GENERATED ALWAYS AS (record->>'PaymentID') STORED NOT NULL,
    FOREIGN KEY (merchant_id, environment, payment_id) REFERENCES tender_payments
);
CREATE INDEX tender_audit_by_payment ON tender_audit (merchant_id, environment, payment_id, seq);
CREATE TABLE tender_outbox (
    seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    record jsonb NOT NULL,
    merchant_id text GENERATED ALWAYS AS (record #>> '{Scope,MerchantID}') STORED NOT NULL,
    environment text GENERATED ALWAYS AS (record #>> '{Scope,Environment}') STORED NOT NULL,
    entry_id text GENERATED ALWAYS AS (record->>'ID') STORED NOT NULL,
    payment_id text GENERATED ALWAYS AS (record->>'PaymentID') STORED NOT NULL,
    consumed_at timestamptz,
    UNIQUE (merchant_id, environment, entry_id),
    FOREIGN KEY (merchant_id, environment, payment_id) REFERENCES tender_payments
);
CREATE INDEX tender_outbox_ready ON tender_outbox (merchant_id, environment, seq) WHERE consumed_at IS NULL;

-- Verified evidence may arrive before its local operation is correlated.
CREATE TABLE tender_provider_events (
    merchant_id text NOT NULL,
    environment text NOT NULL,
    provider text NOT NULL,
    merchant_account text NOT NULL,
    event_id text NOT NULL,
    evidence jsonb NOT NULL,
    received_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    processed_at timestamptz,
    PRIMARY KEY (merchant_id, environment, provider, merchant_account, event_id)
);
CREATE INDEX tender_events_pending ON tender_provider_events (received_at) WHERE processed_at IS NULL;

-- Preserve unmatched report lines. A run may revisit a previous import.
CREATE TABLE tender_settlements (
    merchant_id text NOT NULL,
    environment text NOT NULL,
    provider text NOT NULL,
    merchant_account text NOT NULL,
    report_id text NOT NULL,
    line_id text NOT NULL,
    provider_reference text NOT NULL,
    entry_kind text NOT NULL,
    currency text NOT NULL,
    gross_minor bigint NOT NULL,
    fee_minor bigint NOT NULL,
    net_minor bigint NOT NULL,
    payout_reference text,
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (merchant_id, environment, provider, merchant_account, report_id, line_id)
);
CREATE INDEX tender_settlement_refs ON tender_settlements (merchant_id, environment, provider, merchant_account, provider_reference);
CREATE TABLE tender_reconciliation_runs (
    merchant_id text NOT NULL,
    environment text NOT NULL,
    provider text NOT NULL,
    merchant_account text NOT NULL,
    run_id text NOT NULL,
    report_id text NOT NULL,
    started_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    finished_at timestamptz,
    PRIMARY KEY (merchant_id, environment, provider, merchant_account, run_id)
);
CREATE TABLE tender_reconciliation_history (
    seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id text NOT NULL,
    environment text NOT NULL,
    provider text NOT NULL,
    merchant_account text NOT NULL,
    run_id text NOT NULL,
    line_id text NOT NULL,
    payment_id text,
    outcome text NOT NULL,
    evidence jsonb NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (merchant_id, environment, provider, merchant_account, run_id) REFERENCES tender_reconciliation_runs,
    FOREIGN KEY (merchant_id, environment, payment_id) REFERENCES tender_payments
);
CREATE INDEX tender_reconciliation_by_run ON tender_reconciliation_history (merchant_id, environment, provider, merchant_account, run_id, seq);

COMMIT;
