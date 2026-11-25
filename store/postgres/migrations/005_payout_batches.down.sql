BEGIN;
DROP TABLE tender_reconciliation_payout_history;
DROP TABLE tender_reconciliation_inputs;
ALTER TABLE tender_settlements
    DROP COLUMN modification_reference,
    DROP COLUMN original_reference,
    DROP COLUMN source_section_id,
    DROP COLUMN source_part_id,
    DROP COLUMN batch_id;
COMMIT;
