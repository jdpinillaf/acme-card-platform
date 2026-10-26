-- Load fix (Sprint 7): an account's customer and currency, learned once (from account snapshots
-- or one accounts lookup) instead of asking accounts for every event.
CREATE TABLE account_directory (
    account_id  text        PRIMARY KEY,
    customer_id text        NOT NULL,
    currency    char(3)     NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now()
);
