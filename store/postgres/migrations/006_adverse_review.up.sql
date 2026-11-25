BEGIN;

-- A notice-only adverse event is linked to one authenticated inbox delivery
-- and one payment on the exact provider/account route. The immutable record
-- excludes raw provider payloads and customer identifiers.
CREATE TABLE tender_adverse_reviews (
    seq bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    record jsonb NOT NULL,
    merchant_id text GENERATED ALWAYS AS (record #>> '{Scope,MerchantID}') STORED NOT NULL,
    environment text GENERATED ALWAYS AS (record #>> '{Scope,Environment}') STORED NOT NULL,
    provider text GENERATED ALWAYS AS (record->>'Provider') STORED NOT NULL,
    merchant_account text GENERATED ALWAYS AS (record->>'MerchantAccount') STORED NOT NULL,
    event_id text GENERATED ALWAYS AS (record->>'ProviderEventID') STORED NOT NULL,
    payment_id text GENERATED ALWAYS AS (record->>'PaymentID') STORED NOT NULL,
    decision jsonb,
    PRIMARY KEY (merchant_id, environment, provider, merchant_account, event_id),
    FOREIGN KEY (merchant_id, environment, provider, merchant_account, event_id)
        REFERENCES tender_provider_events (merchant_id, environment, provider, merchant_account, event_id),
    FOREIGN KEY (merchant_id, environment, provider, merchant_account, payment_id)
        REFERENCES tender_payments (merchant_id, environment, provider, merchant_account, payment_id)
);
CREATE INDEX tender_adverse_pending ON tender_adverse_reviews
    (merchant_id, environment, seq) WHERE decision IS NULL;
CREATE INDEX tender_adverse_by_payment ON tender_adverse_reviews
    (merchant_id, environment, provider, merchant_account, payment_id, seq);

CREATE TABLE tender_adverse_review_history (
    seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id text NOT NULL,
    environment text NOT NULL,
    provider text NOT NULL,
    merchant_account text NOT NULL,
    event_id text NOT NULL,
    decision jsonb NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (merchant_id, environment, provider, merchant_account, event_id),
    FOREIGN KEY (merchant_id, environment, provider, merchant_account, event_id)
        REFERENCES tender_adverse_reviews (merchant_id, environment, provider, merchant_account, event_id)
);

COMMIT;
