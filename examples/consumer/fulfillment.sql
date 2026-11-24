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
