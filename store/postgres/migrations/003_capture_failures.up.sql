BEGIN;

-- A scheme rejection may arrive after the collection notice was consumed.
-- Keep the host's review/compensation queue independent of fulfillment.
CREATE TABLE tender_capture_failures (
    seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    record jsonb NOT NULL,
    merchant_id text GENERATED ALWAYS AS (record #>> '{Scope,MerchantID}') STORED NOT NULL,
    environment text GENERATED ALWAYS AS (record #>> '{Scope,Environment}') STORED NOT NULL,
    operation_id text GENERATED ALWAYS AS (record->>'OperationID') STORED NOT NULL,
    reviewed_at timestamptz,
    UNIQUE (merchant_id, environment, operation_id),
    FOREIGN KEY (merchant_id, environment, operation_id) REFERENCES tender_operations
);
CREATE INDEX tender_capture_failures_pending ON tender_capture_failures
    (merchant_id, environment, seq) WHERE reviewed_at IS NULL;

CREATE INDEX tender_provider_evidence_lookup ON tender_provider_events
    USING gin (evidence jsonb_path_ops);

COMMIT;
