BEGIN;

ALTER TABLE tender_reconciliation_history DROP CONSTRAINT tender_reconciliation_payment_route;
ALTER TABLE tender_payments DROP CONSTRAINT tender_payment_route;
ALTER TABLE tender_payments ADD CONSTRAINT tender_payments_merchant_id_environment_provider_merchant_a_key
    UNIQUE (merchant_id, environment, provider, merchant_account, provider_reference);
ALTER TABLE tender_operations ADD CONSTRAINT tender_operations_merchant_id_environment_payment_id_provid_key
    UNIQUE (merchant_id, environment, payment_id, provider_reference);
DROP INDEX tender_payment_reference, tender_operation_reference, tender_settlement_refs;
CREATE INDEX tender_settlement_refs ON tender_settlements
    (merchant_id, environment, provider, merchant_account, provider_reference);
DROP FUNCTION tender_reference_digest(text);

COMMIT;
