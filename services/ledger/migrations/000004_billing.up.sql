-- Sprint 4: business date on journal entries, income sub-accounts, sealed billing periods.
ALTER TABLE journal_entries ADD COLUMN effective_at timestamptz;
ALTER TABLE journal_entries DISABLE TRIGGER journal_entries_append_only;
UPDATE journal_entries SET effective_at = posted_at;
ALTER TABLE journal_entries ENABLE TRIGGER journal_entries_append_only;
ALTER TABLE journal_entries ALTER COLUMN effective_at SET NOT NULL, ALTER COLUMN effective_at SET DEFAULT now();
CREATE INDEX journal_entries_effective_idx ON journal_entries (account_id, effective_at, id);

ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_sub_account_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_sub_account_check
    CHECK (sub_account IN ('cash_clearing','collateral_held','credit_available','receivable','interest_income','fee_income'));
INSERT INTO ledger_accounts (account_id, sub_account, currency)
    SELECT DISTINCT account_id, s, currency FROM ledger_accounts, unnest(ARRAY['interest_income','fee_income']) s
    ON CONFLICT DO NOTHING;
INSERT INTO balances (account_id, sub_account)
    SELECT account_id, sub_account FROM ledger_accounts WHERE sub_account IN ('interest_income','fee_income')
    ON CONFLICT DO NOTHING;

-- Billing seals a period once its statement is closed: later postings never get an earlier effective_at.
CREATE TABLE account_seals (
    account_id     uuid        PRIMARY KEY,
    sealed_through timestamptz NOT NULL
);
