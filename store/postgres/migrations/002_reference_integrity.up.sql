BEGIN;

-- For a UTF-8 database the SHA-256 of the UTF-8 bytes is independent of
-- session settings. Fixed-size index entries support long opaque references.
CREATE FUNCTION tender_reference_digest(reference text) RETURNS bytea
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
RETURN pg_catalog.sha256(pg_catalog.convert_to(reference, 'UTF8'));

CREATE UNIQUE INDEX tender_payment_reference ON tender_payments
    (merchant_id, environment, provider, merchant_account, tender_reference_digest(provider_reference));
ALTER TABLE tender_payments DROP CONSTRAINT tender_payments_merchant_id_environment_provider_merchant_a_key;
CREATE UNIQUE INDEX tender_operation_reference ON tender_operations
    (merchant_id, environment, payment_id, tender_reference_digest(provider_reference));
ALTER TABLE tender_operations DROP CONSTRAINT tender_operations_merchant_id_environment_payment_id_provid_key;
DROP INDEX tender_settlement_refs;
CREATE INDEX tender_settlement_refs ON tender_settlements
    (merchant_id, environment, provider, merchant_account, tender_reference_digest(provider_reference));

ALTER TABLE tender_payments ADD CONSTRAINT tender_payment_route
    UNIQUE (merchant_id, environment, provider, merchant_account, payment_id);
ALTER TABLE tender_reconciliation_history ADD CONSTRAINT tender_reconciliation_payment_route
    FOREIGN KEY (merchant_id, environment, provider, merchant_account, payment_id)
    REFERENCES tender_payments (merchant_id, environment, provider, merchant_account, payment_id);

COMMIT;
