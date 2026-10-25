-- Customer repayments (Sprint 3). Mirrors deposits: intent → bank settlement saga → optional return.
CREATE TABLE payments (
    id                   uuid        PRIMARY KEY,
    account_id           uuid        NOT NULL REFERENCES accounts(id),
    amount_minor         bigint      NOT NULL CHECK (amount_minor >= 100),
    currency             char(3)     NOT NULL,
    status               text        NOT NULL CHECK (status IN ('pending','settling','settled','failed','expired','returning','returned')),
    external_ref         text        UNIQUE,
    return_ref           text        UNIQUE,
    idempotency_key_hash char(64),
    journal_entry_id     uuid,
    return_journal_id    uuid,
    created_at           timestamptz NOT NULL,
    settling_at          timestamptz,
    settled_at           timestamptz,
    returning_at         timestamptz,
    returned_at          timestamptz
);
CREATE UNIQUE INDEX payments_idempotency_key_hash_uq ON payments (idempotency_key_hash) WHERE idempotency_key_hash IS NOT NULL;
CREATE INDEX payments_account_idx ON payments (account_id, created_at DESC, id DESC);
CREATE INDEX payments_pending_idx ON payments (created_at) WHERE status = 'pending';
CREATE INDEX payments_settling_idx ON payments (settling_at) WHERE status = 'settling';
CREATE INDEX payments_returning_idx ON payments (returning_at) WHERE status = 'returning';

-- System restriction set by a returned payment that leaves exposure above the limit.
ALTER TABLE accounts ADD COLUMN restricted_reason text CHECK (restricted_reason IN ('payment_returned'));
ALTER TABLE accounts ADD COLUMN restricted_at timestamptz;
CREATE INDEX accounts_restricted_idx ON accounts (restricted_at) WHERE restricted_reason IS NOT NULL;
