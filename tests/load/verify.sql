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
-- Signed exposure (ADR-018/019): holds + receivable ≤ limit, except an overage explained by returned
-- payments (accepted risk) or by billing charges (interest + fees − waivers), which must instead be
-- restricted (checked below).
SELECT 'exposure > limit (not explained by returns or charges)', count(*) FROM (
  SELECT b.account_id FROM ledger.balances b
  JOIN ledger.balances r ON r.account_id=b.account_id AND r.sub_account='receivable'
  LEFT JOIN (SELECT account_id, sum(amount_minor) h FROM ledger.holds WHERE status='active' GROUP BY 1) h ON h.account_id=b.account_id
  LEFT JOIN (SELECT account_id, sum(amount_minor) s FROM accounts.payments WHERE status IN ('returning','returned') GROUP BY 1) rt ON rt.account_id=b.account_id
  LEFT JOIN (SELECT account_id, sum(amount_minor - waived_minor) s FROM billing.charges WHERE status='posted' GROUP BY 1) ch ON ch.account_id=b.account_id
  WHERE b.sub_account='collateral_held'
    AND coalesce(h.h,0) + r.raw_balance_minor + b.raw_balance_minor > coalesce(rt.s, 0) + coalesce(ch.s, 0)) x
UNION ALL
SELECT 'over limit but not restricted',       count(*) FROM accounts.accounts a
  JOIN ledger.balances r ON r.account_id=a.id AND r.sub_account='receivable'
  LEFT JOIN (SELECT account_id, sum(amount_minor) h FROM ledger.holds WHERE status='active' GROUP BY 1) h ON h.account_id=a.id
  WHERE coalesce(h.h,0) + r.raw_balance_minor > a.credit_limit_minor AND a.restricted_reason IS NULL AND a.delinquency <> 'charged_off'
  -- billing restricts after its catch-up; allow the per-minute sweep to get there
  AND a.updated_at < now() - interval '2 minutes'
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
                                            + (SELECT count(*) FROM accounts.refunds WHERE status='pending' AND created_at < now() - interval '5 minutes')
UNION ALL
-- ---------- Sprint 4: billing (ADR-019) ----------
SELECT 'statement new balance ≠ ledger at close', count(*) FROM billing.statements s
  WHERE s.new_balance_minor <> (SELECT coalesce(sum(CASE WHEN p.side='debit' THEN p.amount_minor ELSE -p.amount_minor END), 0)
    FROM ledger.journal_entries j JOIN ledger.postings p ON p.journal_entry_id=j.id AND p.sub_account='receivable'
    WHERE j.account_id=s.account_id AND j.effective_at < s.period_end)
UNION ALL
SELECT 'statement lines/buckets do not add up', count(*) FROM billing.statements
  WHERE previous_balance_minor + purchases_minor - refunds_minor - payments_minor + returns_minor + interest_minor + fees_minor
        - waivers_minor - collateral_applied_minor <> new_balance_minor
     OR principal_minor + interest_bucket_minor + fee_bucket_minor <> new_balance_minor
UNION ALL
SELECT 'statement cycles not contiguous',     count(*) FROM (SELECT account_id FROM billing.statements GROUP BY 1 HAVING max(cycle) <> count(*)) x
UNION ALL
SELECT 'more than one interest/fee per cycle', count(*) FROM (SELECT account_id, kind, cycle FROM billing.charges GROUP BY 1,2,3 HAVING count(*) > 1) x
UNION ALL
SELECT 'waived ≠ Σ waivers or > charged',     count(*) FROM billing.charges c
  LEFT JOIN (SELECT charge_id, sum(amount_minor) s FROM billing.waivers WHERE status IN ('pending','posted') GROUP BY 1) w ON w.charge_id=c.id
  WHERE c.waived_minor <> coalesce(w.s, 0) OR c.waived_minor > c.amount_minor
UNION ALL
SELECT 'charge ≠ ledger journal',             count(*) FROM (
  SELECT c.id FROM billing.charges c
  FULL JOIN (SELECT j.reference, x.amount_minor FROM ledger.journal_entries j JOIN ledger.postings x ON x.journal_entry_id=j.id AND x.side='debit'
             WHERE j.entry_type IN ('interest_charged','fee_charged')) l ON l.reference = c.reference
  WHERE (c.status='posted' AND (l.reference IS NULL OR l.amount_minor <> c.amount_minor))
     OR (l.reference IS NOT NULL AND (c.id IS NULL OR c.status <> 'posted' AND c.status <> 'pending'))) x
UNION ALL
SELECT 'waiver ≠ ledger journal',             count(*) FROM (
  SELECT w.id FROM billing.waivers w
  FULL JOIN (SELECT j.reference, x.amount_minor FROM ledger.journal_entries j JOIN ledger.postings x ON x.journal_entry_id=j.id AND x.side='debit'
             WHERE j.entry_type IN ('interest_waived','fee_waived')) l ON l.reference = 'waiver:'||w.id
  WHERE (w.status='posted' AND (l.reference IS NULL OR l.amount_minor <> w.amount_minor))
     OR (l.reference IS NOT NULL AND (w.id IS NULL OR w.status = 'failed'))) x
UNION ALL
SELECT 'charged-off account with active holds', count(*) FROM accounts.accounts a JOIN ledger.holds h ON h.account_id=a.id AND h.status='active'
  WHERE a.delinquency='charged_off' AND a.charge_off_pending_at IS NULL
UNION ALL
SELECT 'billing work stuck > 5 min',          (SELECT count(*) FROM billing.charges WHERE status='pending' AND created_at < now() - interval '5 minutes')
                                            + (SELECT count(*) FROM billing.waivers WHERE status='pending' AND created_at < now() - interval '5 minutes')
                                            + (SELECT count(*) FROM accounts.accounts WHERE charge_off_pending_at < now() - interval '5 minutes')
UNION ALL
-- ---------- Sprint 5: close and payouts (ADR-020) ----------
SELECT 'closed account still holding money',  count(*) FROM accounts.accounts a
  JOIN ledger.balances c ON c.account_id=a.id AND c.sub_account='collateral_held'
  JOIN ledger.balances r ON r.account_id=a.id AND r.sub_account='receivable'
  WHERE a.state='closed' AND (c.raw_balance_minor < 0 OR r.raw_balance_minor < 0)
    AND NOT EXISTS (SELECT 1 FROM accounts.payouts p WHERE p.account_id=a.id AND p.status IN ('pending','settling','failed'))
    AND a.updated_at < now() - interval '2 minutes'
UNION ALL
SELECT 'payout journal ≠ settled attempt',    count(*) FROM (
  SELECT t.id FROM accounts.payout_attempts t JOIN accounts.payouts p ON p.id=t.payout_id
  FULL JOIN (SELECT j.reference, x.amount_minor FROM ledger.journal_entries j JOIN ledger.postings x ON x.journal_entry_id=j.id AND x.side='credit'
             WHERE j.entry_type='payout_settled') l ON l.reference = 'payout:'||t.id
  WHERE (t.status='settled' AND (l.reference IS NULL OR l.amount_minor <> p.collateral_minor + p.credit_minor))
     OR (l.reference IS NOT NULL AND (t.id IS NULL OR t.status NOT IN ('settling','settled')))) x
UNION ALL
SELECT 'collateral paid out > deposited',     count(*) FROM (
  SELECT p.account_id FROM accounts.payouts p WHERE p.status='settled' GROUP BY 1
  HAVING sum(p.collateral_minor) > (SELECT coalesce(sum(d.amount_minor),0) FROM accounts.deposits d WHERE d.account_id=p.account_id AND d.status='settled')) x
UNION ALL
SELECT 'closed account with in-flight work',  count(*) FROM accounts.accounts a WHERE a.state='closed' AND (
     EXISTS (SELECT 1 FROM accounts.authorizations z WHERE z.account_id=a.id AND (z.status IN ('approved','pending') OR z.pending_op IS NOT NULL))
  OR EXISTS (SELECT 1 FROM accounts.payments z WHERE z.account_id=a.id AND z.status IN ('pending','settling'))
  OR EXISTS (SELECT 1 FROM accounts.deposits z WHERE z.account_id=a.id AND z.status IN ('pending','settling')))
UNION ALL
SELECT 'payment returned after its close payout', count(*) FROM accounts.payments m JOIN accounts.payouts p ON p.account_id=m.account_id AND p.kind='close'
  WHERE m.status IN ('returning','returned') AND m.returning_at > p.created_at
UNION ALL
SELECT 'cancelled account with an open deposit', count(*) FROM accounts.accounts a JOIN accounts.deposits d ON d.account_id=a.id
  WHERE a.state='cancelled' AND d.status IN ('pending','settling','settled')
UNION ALL
SELECT 'close saga stuck > 10 min',           count(*) FROM accounts.accounts
  WHERE state='closing' AND close_stage IN ('final_statement','applying') AND close_stage_at < now() - interval '10 minutes';
