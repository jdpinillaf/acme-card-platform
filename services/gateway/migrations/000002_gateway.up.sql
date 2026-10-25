-- Sliding-window rate limit counters (one row per key per fixed window).
CREATE TABLE rate_limit_windows (
    bucket       text        NOT NULL,
    window_start timestamptz NOT NULL,
    hits         integer     NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket, window_start)
);

-- Rejected webhooks collapse to one row per (source, window) so floods don't bloat storage.
CREATE TABLE webhook_rejections (
    source       text        NOT NULL,
    window_start timestamptz NOT NULL,
    last_reason  text        NOT NULL,
    attempts     integer     NOT NULL DEFAULT 1,
    first_seen   timestamptz NOT NULL,
    last_seen    timestamptz NOT NULL,
    PRIMARY KEY (source, window_start)
);

CREATE TABLE outbox (
    seq          bigserial   PRIMARY KEY,
    event_id     uuid        NOT NULL UNIQUE,
    payload      jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX outbox_unpublished_idx ON outbox (seq) WHERE published_at IS NULL;
