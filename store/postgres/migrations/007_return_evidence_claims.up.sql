BEGIN;

-- One processed adverse event can corroborate one immutable settlement row.
-- Replays of that row are allowed; another report cannot spend the event again.
CREATE TABLE tender_reconciliation_return_claims (
    merchant_id text NOT NULL,
    environment text NOT NULL,
    provider text NOT NULL,
    merchant_account text NOT NULL,
    event_id text NOT NULL,
    report_id text NOT NULL,
    line_id text NOT NULL,
    PRIMARY KEY (merchant_id, environment, provider, merchant_account, event_id),
    UNIQUE (merchant_id, environment, provider, merchant_account, report_id, line_id),
    FOREIGN KEY (merchant_id, environment, provider, merchant_account, event_id)
        REFERENCES tender_adverse_reviews,
    FOREIGN KEY (merchant_id, environment, provider, merchant_account, report_id, line_id)
        REFERENCES tender_settlements
);

COMMIT;
