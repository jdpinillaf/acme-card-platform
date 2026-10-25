CREATE TABLE accounts (
    id                   uuid        PRIMARY KEY,
    customer_id          text        NOT NULL,
    currency             char(3)     NOT NULL,
    state                text        NOT NULL CHECK (state IN ('pending_collateral','active','closed')),
    frozen               boolean     NOT NULL DEFAULT false,
    collateral_minor     bigint      NOT NULL DEFAULT 0 CHECK (collateral_minor >= 0),
    credit_limit_minor   bigint      NOT NULL DEFAULT 0 CHECK (credit_limit_minor >= 0),
    purchase_limit_minor bigint      NOT NULL DEFAULT 0 CHECK (purchase_limit_minor >= 0), -- 0 = no customer cap
    version              bigint      NOT NULL DEFAULT 1,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (customer_id, currency),
    -- No overdraft in MVP: the limit can never exceed the collateral that backs it.
    CHECK (credit_limit_minor <= collateral_minor)
);

CREATE TABLE deposits (
    id             uuid        PRIMARY KEY,
    account_id     uuid        NOT NULL REFERENCES accounts(id),
    amount_minor   bigint      NOT NULL CHECK (amount_minor > 0),
    currency       char(3)     NOT NULL,
    status         text        NOT NULL CHECK (status IN ('pending','settled','failed')),
    external_ref   text        UNIQUE,
    journal_entry_id uuid,
    created_at     timestamptz NOT NULL DEFAULT now(),
    settled_at     timestamptz
);
CREATE INDEX deposits_account_idx ON deposits (account_id, created_at);
