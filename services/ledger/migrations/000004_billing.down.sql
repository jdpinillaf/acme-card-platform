DROP TABLE account_seals;
DELETE FROM balances WHERE sub_account IN ('interest_income','fee_income');
DELETE FROM ledger_accounts WHERE sub_account IN ('interest_income','fee_income');
ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_sub_account_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_sub_account_check
    CHECK (sub_account IN ('cash_clearing','collateral_held','credit_available','receivable'));
DROP INDEX journal_entries_effective_idx;
ALTER TABLE journal_entries DROP COLUMN effective_at;
