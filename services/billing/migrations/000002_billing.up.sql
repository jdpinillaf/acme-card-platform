-- Billing (Sprint 4, ADR-019). billing_accounts rows serialize catch-ups per account (FOR UPDATE).
CREATE TABLE billing_accounts (
    account_id uuid        PRIMARY KEY,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE statements (
    id                       uuid        PRIMARY KEY,
    account_id               uuid        NOT NULL,
    cycle                    int         NOT NULL CHECK (cycle >= 1),
    currency                 char(3)     NOT NULL,
    period_start             timestamptz NOT NULL,
    period_end               timestamptz NOT NULL,
    due_date                 timestamptz,
    previous_balance_minor   bigint      NOT NULL,
    purchases_minor          bigint      NOT NULL,
    refunds_minor            bigint      NOT NULL,
    payments_minor           bigint      NOT NULL,
    returns_minor            bigint      NOT NULL,
    interest_minor           bigint      NOT NULL,
    fees_minor               bigint      NOT NULL,
    waivers_minor            bigint      NOT NULL,
    collateral_applied_minor bigint      NOT NULL,
    new_balance_minor        bigint      NOT NULL,
    minimum_due_minor        bigint      NOT NULL CHECK (minimum_due_minor >= 0),
    past_due_minor           bigint      NOT NULL,
    principal_minor          bigint      NOT NULL,
    interest_bucket_minor    bigint      NOT NULL,
    fee_bucket_minor         bigint      NOT NULL,
    grace                    boolean     NOT NULL,
    balance_only             boolean     NOT NULL,
    s3_key                   text        NOT NULL,
    sha256                   char(64)    NOT NULL,
    generated_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, cycle)
);

-- Interest and late-fee charges. The ledger reference is the idempotency anchor.
CREATE TABLE charges (
    id           uuid        PRIMARY KEY,
    account_id   uuid        NOT NULL,
    kind         text        NOT NULL CHECK (kind IN ('interest','fee')),
    cycle        int         NOT NULL,
    reference    text        NOT NULL UNIQUE,
    amount_minor bigint      NOT NULL CHECK (amount_minor > 0),
    waived_minor bigint      NOT NULL DEFAULT 0 CHECK (waived_minor >= 0 AND waived_minor <= amount_minor),
    effective_at timestamptz NOT NULL,
    status       text        NOT NULL CHECK (status IN ('pending','posted','failed')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    posted_at    timestamptz
);
CREATE INDEX charges_account_idx ON charges (account_id, effective_at);
CREATE INDEX charges_pending_idx ON charges (created_at) WHERE status = 'pending';

-- Support waivers of a charge (Sprint 4, Task 7).
CREATE TABLE waivers (
    id                   uuid        PRIMARY KEY,
    charge_id            uuid        NOT NULL REFERENCES charges(id),
    account_id           uuid        NOT NULL,
    amount_minor         bigint      NOT NULL CHECK (amount_minor > 0),
    reason               text        NOT NULL CHECK (length(reason) BETWEEN 1 AND 200),
    actor                text        NOT NULL,
    idempotency_key_hash char(64)    UNIQUE,
    effective_at         timestamptz NOT NULL,
    status               text        NOT NULL CHECK (status IN ('pending','posted','failed')),
    created_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX waivers_account_idx ON waivers (account_id);
