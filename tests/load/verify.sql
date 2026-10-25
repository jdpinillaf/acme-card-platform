-- Post-load invariants. Every row must report 0.
SELECT 'journal reference used twice'         AS check, count(*) AS violations FROM (SELECT reference FROM ledger.journal_entries GROUP BY reference HAVING count(*) > 1) x
UNION ALL
SELECT 'settled deposits without journal',    count(*) FROM accounts.deposits d LEFT JOIN ledger.journal_entries j ON j.reference = 'deposit:' || d.id WHERE d.status = 'settled' AND j.id IS NULL
UNION ALL
SELECT 'deposit journals without settled deposit', count(*) FROM ledger.journal_entries j LEFT JOIN accounts.deposits d ON 'deposit:' || d.id = j.reference AND d.status = 'settled' WHERE j.entry_type = 'collateral_deposit_settled' AND d.id IS NULL
UNION ALL
SELECT 'unbalanced journal entries',          count(*) FROM (SELECT journal_entry_id FROM ledger.postings GROUP BY 1 HAVING sum(CASE WHEN side='debit' THEN amount_minor ELSE -amount_minor END) <> 0) x
UNION ALL
SELECT 'limit != ledger collateral',          count(*) FROM accounts.accounts a JOIN ledger.balances b ON b.account_id = a.id AND b.sub_account = 'collateral_held' WHERE a.credit_limit_minor <> -b.raw_balance_minor
UNION ALL
SELECT 'balance snapshot drift',              count(*) FROM ledger.balances b LEFT JOIN (SELECT account_id, sub_account, sum(CASE WHEN side='debit' THEN amount_minor ELSE -amount_minor END) s FROM ledger.postings GROUP BY 1,2) p USING (account_id, sub_account) WHERE b.raw_balance_minor <> coalesce(p.s, 0)
UNION ALL
SELECT 'idempotency keys stuck in_progress',  count(*) FROM gateway.idempotency_keys WHERE status = 'in_progress' AND created_at < now() - interval '1 minute'
UNION ALL
SELECT 'deposits stuck in settling',          count(*) FROM accounts.deposits WHERE status = 'settling' AND created_at < now() - interval '1 minute' 
UNION ALL
SELECT 'holds + max(receivable,0) > limit',   count(*) FROM (
  SELECT b.account_id FROM ledger.balances b
  LEFT JOIN (SELECT account_id, sum(amount_minor) h FROM ledger.holds WHERE status='active' GROUP BY 1) h USING (account_id)
  JOIN ledger.balances r ON r.account_id=b.account_id AND r.sub_account='receivable'
  WHERE b.sub_account='collateral_held' AND coalesce(h.h,0) + greatest(r.raw_balance_minor,0) > -b.raw_balance_minor) x
UNION ALL
SELECT 'captured auth without captured hold', count(*) FROM accounts.authorizations a LEFT JOIN ledger.holds h ON h.reference='auth:'||a.network_ref AND h.status='captured' WHERE a.status='captured' AND h.id IS NULL
UNION ALL
SELECT 'active hold on non-approved auth',    count(*) FROM ledger.holds h JOIN accounts.authorizations a ON h.reference='auth:'||a.network_ref WHERE h.status='active' AND a.status<>'approved'
UNION ALL
SELECT 'refunds exceed captured',             count(*) FROM (SELECT a.id FROM accounts.authorizations a JOIN accounts.refunds r ON r.authorization_id=a.id GROUP BY a.id, a.captured_minor HAVING sum(r.amount_minor) > a.captured_minor) x
UNION ALL
SELECT 'more than one capture journal per auth', count(*) FROM (SELECT reference FROM ledger.journal_entries WHERE entry_type='purchase_captured' GROUP BY reference HAVING count(*)>1) x;
