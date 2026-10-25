CREATE TYPE idem_status AS ENUM ('in_progress', 'completed');
CREATE TYPE idem_outcome AS ENUM ('success', 'client_error', 'server_error', 'timeout', 'rolled_back');

-- Insert-first idempotency store. key_hash = sha256(subject || key). The inserting transaction
-- holds the row lock until the request completes, so concurrent duplicates block on the unique
-- index, then observe the completed row and replay it.
CREATE TABLE idempotency_keys (
    key_hash      char(64)     PRIMARY KEY,
    request_hash  char(64)     NOT NULL,
    status        idem_status  NOT NULL DEFAULT 'in_progress',
    response_code integer,
    response_body bytea,
    outcome       idem_outcome,
    created_at    timestamptz  NOT NULL DEFAULT now(),
    completed_at  timestamptz,
    expires_at    timestamptz  NOT NULL
);
CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);
