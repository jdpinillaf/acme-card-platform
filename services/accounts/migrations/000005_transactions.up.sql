-- Card network transactions (Sprint 2). One row per authorization attempt, approved or declined.
CREATE TABLE authorizations (
    id                   uuid        PRIMARY KEY,
    account_id           uuid        NOT NULL REFERENCES accounts(id),
    network_ref          text        NOT NULL UNIQUE,
    request_hash         char(64)    NOT NULL,
    amount_minor         bigint      NOT NULL CHECK (amount_minor > 0),
    currency             char(3)     NOT NULL,
    merchant_id          text        NOT NULL,
    merchant_name        text        NOT NULL,
    mcc                  text        NOT NULL,
    status               text        NOT NULL CHECK (status IN ('approved','declined','captured','reversed','expired')),
    decline_reason       text,
    captured_minor       bigint      NOT NULL DEFAULT 0 CHECK (captured_minor >= 0),
    refunded_minor       bigint      NOT NULL DEFAULT 0 CHECK (refunded_minor >= 0),
    capture_network_ref  text        UNIQUE,
    reversal_network_ref text        UNIQUE,
    created_at           timestamptz NOT NULL,
    expires_at           timestamptz,
    captured_at          timestamptz,
    reversed_at          timestamptz,
    CHECK (captured_minor <= amount_minor),
    CHECK (refunded_minor <= captured_minor),
    CHECK ((status = 'declined') = (decline_reason IS NOT NULL))
);
CREATE INDEX authorizations_account_idx ON authorizations (account_id, created_at DESC, id DESC);
CREATE INDEX authorizations_expiry_idx ON authorizations (expires_at) WHERE status = 'approved';

CREATE TABLE refunds (
    id               uuid        PRIMARY KEY,
    authorization_id uuid        NOT NULL REFERENCES authorizations(id),
    network_ref      text        NOT NULL UNIQUE,
    amount_minor     bigint      NOT NULL CHECK (amount_minor > 0),
    created_at       timestamptz NOT NULL
);
CREATE INDEX refunds_authorization_idx ON refunds (authorization_id);

-- Velocity limiter windows (pkg/ratelimit), keyed "card:<account_id>".
CREATE TABLE rate_limit_windows (
    bucket       text        NOT NULL,
    window_start timestamptz NOT NULL,
    hits         integer     NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket, window_start)
);
