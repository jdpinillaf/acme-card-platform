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
SELECT 'deposits stuck in settling',          count(*) FROM accounts.deposits WHERE status = 'settling' AND created_at < now() - interval '1 minute' 
UNION ALL
-- Signed exposure (ADR-018): holds + receivable ≤ limit, except an overage explained by returned
-- payments (accepted risk), which must instead be restricted (checked below).
SELECT 'exposure > limit (not explained by returns)', count(*) FROM (
  SELECT b.account_id FROM ledger.balances b
  JOIN ledger.balances r ON r.account_id=b.account_id AND r.sub_account='receivable'
  LEFT JOIN (SELECT account_id, sum(amount_minor) h FROM ledger.holds WHERE status='active' GROUP BY 1) h ON h.account_id=b.account_id
  LEFT JOIN (SELECT account_id, sum(amount_minor) s FROM accounts.payments WHERE status IN ('returning','returned') GROUP BY 1) rt ON rt.account_id=b.account_id
  WHERE b.sub_account='collateral_held'
    AND coalesce(h.h,0) + r.raw_balance_minor + b.raw_balance_minor > coalesce(rt.s, 0)) x
UNION ALL
SELECT 'over limit but not restricted',       count(*) FROM accounts.accounts a
  JOIN ledger.balances r ON r.account_id=a.id AND r.sub_account='receivable'
  LEFT JOIN (SELECT account_id, sum(amount_minor) h FROM ledger.holds WHERE status='active' GROUP BY 1) h ON h.account_id=a.id
  WHERE coalesce(h.h,0) + r.raw_balance_minor > a.credit_limit_minor AND a.restricted_reason IS NULL
UNION ALL
SELECT 'restricted but within limit > 5 min', count(*) FROM accounts.accounts a
  JOIN ledger.balances r ON r.account_id=a.id AND r.sub_account='receivable'
  LEFT JOIN (SELECT account_id, sum(amount_minor) h FROM ledger.holds WHERE status='active' GROUP BY 1) h ON h.account_id=a.id
  WHERE a.restricted_reason IS NOT NULL AND coalesce(h.h,0) + r.raw_balance_minor <= a.credit_limit_minor AND a.restricted_at < now() - interval '5 minutes'
UNION ALL
SELECT 'payment journal ≠ settled payment',   count(*) FROM (
  SELECT p.id FROM accounts.payments p
  FULL JOIN (SELECT j.reference, x.amount_minor FROM ledger.journal_entries j JOIN ledger.postings x ON x.journal_entry_id=j.id AND x.side='debit'
             WHERE j.entry_type='payment_settled') l ON l.reference = 'payment:'||p.id
  WHERE (p.status IN ('settled','returning','returned') AND (l.reference IS NULL OR l.amount_minor <> p.amount_minor))
     OR (l.reference IS NOT NULL AND (p.id IS NULL OR p.status NOT IN ('settling','settled','returning','returned')))) x
UNION ALL
SELECT 'return journal ≠ returned payment',   count(*) FROM (
  SELECT p.id FROM accounts.payments p
  FULL JOIN (SELECT j.reference, x.amount_minor FROM ledger.journal_entries j JOIN ledger.postings x ON x.journal_entry_id=j.id AND x.side='debit'
             WHERE j.entry_type='payment_returned') l ON l.reference = 'payment_return:'||p.id
  WHERE (p.status = 'returned' AND (l.reference IS NULL OR l.amount_minor <> p.amount_minor))
     OR (l.reference IS NOT NULL AND (p.id IS NULL OR p.status NOT IN ('returning','returned')))) x
UNION ALL
SELECT 'payments stuck settling/returning > 5 min', count(*) FROM accounts.payments
  WHERE (status='settling' AND settling_at < now() - interval '5 minutes') OR (status='returning' AND returning_at < now() - interval '5 minutes')
UNION ALL
-- Every hold must belong to an authorization (catches orphans from lost responses).
SELECT 'hold without authorization',          count(*) FROM ledger.holds h LEFT JOIN accounts.authorizations a ON h.reference = 'auth:'||a.network_ref WHERE a.id IS NULL
UNION ALL
-- Hold state must match authorization state (pending ops and pending auths may lag briefly).
SELECT 'hold/authorization state mismatch',   count(*) FROM accounts.authorizations a LEFT JOIN ledger.holds h ON h.reference = 'auth:'||a.network_ref
  WHERE a.pending_op IS NULL AND a.status <> 'pending' AND NOT (
       (a.status = 'approved' AND h.status = 'active' AND h.amount_minor = a.amount_minor)
    OR (a.status = 'captured' AND h.status = 'captured' AND h.captured_minor = a.captured_minor)
    OR (a.status = 'reversed' AND h.status = 'released')
    OR (a.status = 'expired'  AND h.status = 'expired')
    OR (a.status IN ('declined','failed') AND (h.id IS NULL OR h.status IN ('released','expired'))))
UNION ALL
SELECT 'capture journal ≠ captured authorization', count(*) FROM (
  SELECT a.id FROM accounts.authorizations a
  FULL JOIN (SELECT j.reference, p.amount_minor FROM ledger.journal_entries j JOIN ledger.postings p ON p.journal_entry_id=j.id AND p.side='debit'
             WHERE j.entry_type='purchase_captured') c ON c.reference = 'capture:'||a.id
  WHERE (a.status = 'captured' AND a.pending_op IS NULL AND (c.reference IS NULL OR c.amount_minor <> a.captured_minor))
     OR (c.reference IS NOT NULL AND (a.id IS NULL OR (a.status <> 'captured' AND a.pending_op IS DISTINCT FROM 'capture')))) x
UNION ALL
SELECT 'refund journal ≠ posted refund',      count(*) FROM (
  SELECT r.id FROM accounts.refunds r
  FULL JOIN (SELECT j.reference, p.amount_minor FROM ledger.journal_entries j JOIN ledger.postings p ON p.journal_entry_id=j.id AND p.side='debit'
             WHERE j.entry_type='purchase_refunded') l ON l.reference = 'refund:'||r.network_ref
  WHERE (r.status = 'posted' AND (l.reference IS NULL OR l.amount_minor <> r.amount_minor))
     OR (l.reference IS NOT NULL AND r.id IS NULL)) x
UNION ALL
SELECT 'refunds exceed captured',             count(*) FROM (SELECT a.id FROM accounts.authorizations a JOIN accounts.refunds r ON r.authorization_id=a.id GROUP BY a.id, a.captured_minor HAVING sum(r.amount_minor) > a.captured_minor) x
UNION ALL
SELECT 'refunded_minor ≠ Σ posted refunds',   count(*) FROM accounts.authorizations a LEFT JOIN (SELECT authorization_id, sum(amount_minor) s FROM accounts.refunds WHERE status='posted' GROUP BY 1) r ON r.authorization_id=a.id
  WHERE a.refunded_minor <> coalesce(r.s, 0)
UNION ALL
SELECT 'work stuck pending > 5 min',          (SELECT count(*) FROM accounts.authorizations WHERE (status='pending' AND created_at < now() - interval '5 minutes') OR (pending_op IS NOT NULL AND pending_op_at < now() - interval '5 minutes'))
                                            + (SELECT count(*) FROM accounts.refunds WHERE status='pending' AND created_at < now() - interval '5 minutes');
