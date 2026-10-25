-- Sprint 5: close, cancel and payouts (ADR-020).
ALTER TABLE accounts DROP CONSTRAINT accounts_state_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_state_check CHECK (state IN ('pending_collateral','active','closing','closed','cancelled'));
-- One open account per (customer, currency); closed and cancelled ones free the slot.
ALTER TABLE accounts DROP CONSTRAINT accounts_customer_id_currency_key;
CREATE UNIQUE INDEX accounts_open_customer_currency_uq ON accounts (customer_id, currency) WHERE state NOT IN ('closed','cancelled');

ALTER TABLE accounts
    ADD COLUMN close_stage            text CHECK (close_stage IN ('in_flight','final_statement','awaiting_due','applying','awaiting_returns','payout','closed')),
    ADD COLUMN close_stage_at         timestamptz,
    ADD COLUMN close_requested_at     timestamptz,
    ADD COLUMN close_requested_by     text,
    ADD COLUMN close_reason           text,
    ADD COLUMN destination_ciphertext text,
    ADD COLUMN destination_last4      text,
    ADD COLUMN final_due_at           timestamptz,
    -- collateral applied to the debt at close: amount fixed at reserve, seq makes each application a new reference
    ADD COLUMN close_apply_amount     bigint CHECK (close_apply_amount >= 0),
    ADD COLUMN close_apply_seq        int NOT NULL DEFAULT 0;

ALTER TABLE deposits DROP CONSTRAINT deposits_status_check;
ALTER TABLE deposits ADD CONSTRAINT deposits_status_check CHECK (status IN ('pending','settling','settled','failed','voided'));

-- A payout returns collateral and credit balance; each bank try is an attempt whose id is the
-- external reference the bank echoes.
CREATE TABLE payouts (
    id               uuid        PRIMARY KEY,
    account_id       uuid        NOT NULL REFERENCES accounts(id),
    kind             text        NOT NULL CHECK (kind IN ('close','additional')),
    collateral_minor bigint      NOT NULL CHECK (collateral_minor >= 0),
    credit_minor     bigint      NOT NULL CHECK (credit_minor >= 0),
    currency         char(3)     NOT NULL,
    status           text        NOT NULL CHECK (status IN ('pending','settling','settled','failed')),
    attempt          int         NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL DEFAULT now(),
    settled_at       timestamptz,
    CHECK (collateral_minor + credit_minor > 0)
);
-- At most one open payout per account.
CREATE UNIQUE INDEX payouts_one_open_uq ON payouts (account_id) WHERE status IN ('pending','settling');

CREATE TABLE payout_attempts (
    id               uuid        PRIMARY KEY,
    payout_id        uuid        NOT NULL REFERENCES payouts(id),
    attempt          int         NOT NULL,
    status           text        NOT NULL CHECK (status IN ('pending','settling','settled','failed')),
    effective_at     timestamptz,
    journal_entry_id uuid,
    created_at       timestamptz NOT NULL DEFAULT now(),
    settling_at      timestamptz,
    settled_at       timestamptz,
    UNIQUE (payout_id, attempt)
);
