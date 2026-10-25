-- When a deposit entered 'settling'; lets the reconciler resume settlements whose bank webhook
-- response was lost after the ledger post.
ALTER TABLE deposits ADD COLUMN settling_at timestamptz;
CREATE INDEX deposits_settling_idx ON deposits (settling_at) WHERE status = 'settling';
