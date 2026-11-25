CREATE TABLE consumer_fulfillments (
    merchant_id text NOT NULL,
    environment text NOT NULL,
    order_id text NOT NULL,
    payment_id text NOT NULL,
    entry_id text NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (merchant_id, environment, order_id),
    UNIQUE (merchant_id, environment, entry_id),
    FOREIGN KEY (merchant_id, environment, payment_id) REFERENCES tender_payments
);

-- MOCK-only simulator continuity. These are synthetic session references and
-- decisions, never bank account or mandate data and never payment evidence.
CREATE TABLE consumer_ach_fixtures (
    merchant_id text NOT NULL,
    environment text NOT NULL,
    order_id text NOT NULL,
    payment_id text NOT NULL,
    session_id text NOT NULL,
    merchant_reference text NOT NULL,
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    expires_at timestamptz NOT NULL,
    scenario text NOT NULL CHECK (scenario IN ('', 'validation-refusal', 'pending')),
    decision text NOT NULL DEFAULT '' CHECK (decision IN ('', 'approve', 'deny')),
    returned boolean NOT NULL DEFAULT false,
    PRIMARY KEY (merchant_id, environment, order_id),
    UNIQUE (merchant_id, environment, session_id),
    FOREIGN KEY (merchant_id, environment, payment_id) REFERENCES tender_payments,
    CHECK (NOT returned OR decision = 'approve')
);
