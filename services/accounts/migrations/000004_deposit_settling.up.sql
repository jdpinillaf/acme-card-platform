-- 'settling' reserves a deposit (and its external_ref) before the ledger is posted, so a
-- concurrent 'failed' notice or a reused external_ref can never strand ledger collateral.
ALTER TABLE deposits DROP CONSTRAINT deposits_status_check;
ALTER TABLE deposits ADD CONSTRAINT deposits_status_check CHECK (status IN ('pending','settling','settled','failed'));
