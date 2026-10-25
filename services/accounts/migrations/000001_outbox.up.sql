CREATE TABLE outbox (
    seq          bigserial   PRIMARY KEY,
    event_id     uuid        NOT NULL UNIQUE,
    payload      jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX outbox_unpublished_idx ON outbox (seq) WHERE published_at IS NULL;
