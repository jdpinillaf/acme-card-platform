-- Sprint 7 (ADR-022): customer notifications and partner webhooks.

-- An event is turned into deliveries once: this row is written in the same transaction.
CREATE TABLE processed_events (
    event_id     uuid        PRIMARY KEY,
    processed_at timestamptz NOT NULL DEFAULT now()
);

-- Per customer and notification type; no row = every channel on.
CREATE TABLE preferences (
    customer_id text        NOT NULL,
    type        text        NOT NULL,
    push        boolean     NOT NULL,
    sms         boolean     NOT NULL,
    email       boolean     NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (customer_id, type)
);

-- Partner webhook subscriptions; the secret is sealed (AES-256-GCM, NOTIFY_SECRET_KEY).
CREATE TABLE subscriptions (
    id            uuid        PRIMARY KEY,
    name          text        NOT NULL,
    url           text        NOT NULL,
    event_types   text[]      NOT NULL,
    secret_sealed text        NOT NULL,
    active        boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    disabled_at   timestamptz
);

-- One row per (event, target): push|sms|email or webhook:<subscription id>.
CREATE TABLE deliveries (
    id                uuid        PRIMARY KEY,
    event_id          uuid        NOT NULL,
    event_type        text        NOT NULL,
    notification_type text,
    account_id        text,
    customer_id       text,
    target            text        NOT NULL,
    recipient         text        NOT NULL,
    payload           jsonb,
    text              text,
    status            text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','delivered','failed')),
    attempts          int         NOT NULL DEFAULT 0,
    next_attempt_at   timestamptz NOT NULL DEFAULT now(),
    last_error        text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (event_id, target)
);
CREATE INDEX deliveries_due_idx ON deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX deliveries_target_idx ON deliveries (target, created_at DESC);

-- What the simulated providers "sent"; the simulator reads it per account.
CREATE TABLE inbox (
    id          bigserial   PRIMARY KEY,
    account_id  text        NOT NULL,
    customer_id text        NOT NULL,
    channel     text        NOT NULL,
    type        text        NOT NULL,
    text        text        NOT NULL,
    delivery_id uuid        NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX inbox_account_idx ON inbox (account_id, id DESC);

-- Suspected-fraud window (declines that count) and its once-per-hour throttle.
CREATE TABLE declines (
    account_id text        NOT NULL,
    at         timestamptz NOT NULL
);
CREATE INDEX declines_account_idx ON declines (account_id, at);
CREATE TABLE fraud_alerts (
    account_id text        NOT NULL,
    at         timestamptz NOT NULL
);
CREATE INDEX fraud_alerts_account_idx ON fraud_alerts (account_id, at);

-- The service publishes notification.delivered / notification.failed like every producer.
CREATE TABLE outbox (
    seq          bigserial   PRIMARY KEY,
    event_id     uuid        NOT NULL UNIQUE,
    payload      jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX outbox_unpublished_idx ON outbox (seq) WHERE published_at IS NULL;
CREATE TABLE outbox_sequence (
    source   text   PRIMARY KEY,
    next_seq bigint NOT NULL CHECK (next_seq >= 1)
);
