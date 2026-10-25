-- Idempotency anchor for simulated-clock advances (review I2): the gateway releases a key when the
-- response is a 5xx, so accounts must dedupe a retried advance itself.
CREATE TABLE clock_advances (
    idempotency_key_hash char(64)    PRIMARY KEY,
    account_id           uuid        NOT NULL,
    days                 int         NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now()
);
