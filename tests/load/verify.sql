-- Post-load invariants. Every row must report 0.
SELECT 'deposits settled twice (ledger)'      AS check, count(*) AS violations FROM (SELECT reference FROM ledger.journal_entries GROUP BY reference HAVING count(*) > 1) x
UNION ALL
SELECT 'settled deposits without journal',    count(*) FROM accounts.deposits d LEFT JOIN ledger.journal_entries j ON j.reference = 'deposit:' || d.id WHERE d.status = 'settled' AND j.id IS NULL
UNION ALL
SELECT 'journal entries without settled deposit', count(*) FROM ledger.journal_entries j LEFT JOIN accounts.deposits d ON 'deposit:' || d.id = j.reference AND d.status = 'settled' WHERE d.id IS NULL
UNION ALL
SELECT 'unbalanced journal entries',          count(*) FROM (SELECT journal_entry_id FROM ledger.postings GROUP BY 1 HAVING sum(CASE WHEN side='debit' THEN amount_minor ELSE -amount_minor END) <> 0) x
UNION ALL
SELECT 'limit != ledger collateral',          count(*) FROM accounts.accounts a JOIN ledger.balances b ON b.account_id = a.id AND b.sub_account = 'collateral_held' WHERE a.credit_limit_minor <> -b.raw_balance_minor
UNION ALL
SELECT 'balance snapshot drift',              count(*) FROM ledger.balances b LEFT JOIN (SELECT account_id, sub_account, sum(CASE WHEN side='debit' THEN amount_minor ELSE -amount_minor END) s FROM ledger.postings GROUP BY 1,2) p USING (account_id, sub_account) WHERE b.raw_balance_minor <> coalesce(p.s, 0)
UNION ALL
SELECT 'idempotency keys stuck in_progress',  count(*) FROM gateway.idempotency_keys WHERE status = 'in_progress' AND created_at < now() - interval '1 minute'
UNION ALL
SELECT 'deposits stuck in settling',          count(*) FROM accounts.deposits WHERE status = 'settling' AND created_at < now() - interval '1 minute';
