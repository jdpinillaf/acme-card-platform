CREATE TYPE posting_side AS ENUM ('debit', 'credit');

-- One row per (customer account, sub-account). Currency is fixed per account.
CREATE TABLE ledger_accounts (
    account_id  uuid        NOT NULL,
    sub_account text        NOT NULL CHECK (sub_account IN ('cash_clearing','collateral_held','credit_available','receivable')),
    currency    char(3)     NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, sub_account)
);

-- Immutable journal. reference is the caller's idempotency anchor (e.g. deposit id).
CREATE TABLE journal_entries (
    id            uuid        PRIMARY KEY,
    account_id    uuid        NOT NULL,
    reference     text        NOT NULL UNIQUE,
    entry_type    text        NOT NULL,
    postings_hash char(64)    NOT NULL,
    idempotency_key_hash char(64),
    actor         text,
    posted_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX journal_entries_account_idx ON journal_entries (account_id, posted_at);

CREATE TABLE postings (
    id               bigserial    PRIMARY KEY,
    journal_entry_id uuid         NOT NULL REFERENCES journal_entries(id),
    account_id       uuid         NOT NULL,
    sub_account      text         NOT NULL,
    side             posting_side NOT NULL,
    amount_minor     bigint       NOT NULL CHECK (amount_minor > 0),
    FOREIGN KEY (account_id, sub_account) REFERENCES ledger_accounts(account_id, sub_account)
);
CREATE INDEX postings_entry_idx ON postings (journal_entry_id);

-- Cached balance snapshot. raw_balance_minor = Σdebit − Σcredit. Only mutable table; always
-- updated in the same transaction as the postings it summarizes.
CREATE TABLE balances (
    account_id        uuid        NOT NULL,
    sub_account       text        NOT NULL,
    raw_balance_minor bigint      NOT NULL DEFAULT 0,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, sub_account),
    FOREIGN KEY (account_id, sub_account) REFERENCES ledger_accounts(account_id, sub_account)
);

CREATE FUNCTION ledger_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only (% rejected)', TG_TABLE_NAME, TG_OP;
END;
$$;
CREATE TRIGGER journal_entries_append_only BEFORE UPDATE OR DELETE ON journal_entries FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER postings_append_only BEFORE UPDATE OR DELETE ON postings FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER journal_entries_no_truncate BEFORE TRUNCATE ON journal_entries FOR EACH STATEMENT EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER postings_no_truncate BEFORE TRUNCATE ON postings FOR EACH STATEMENT EXECUTE FUNCTION ledger_append_only();
