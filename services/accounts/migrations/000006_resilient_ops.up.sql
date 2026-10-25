-- Reserve → ledger → finalize for every card network operation (review C1/C3/I1b/I3).
ALTER TABLE authorizations DROP CONSTRAINT authorizations_status_check;
ALTER TABLE authorizations ADD CONSTRAINT authorizations_status_check
    CHECK (status IN ('pending','approved','declined','captured','reversed','expired','failed'));

-- A capture or reversal reserved (ref claimed, unique constraint enforced) but not yet finalized.
ALTER TABLE authorizations ADD COLUMN pending_op text CHECK (pending_op IN ('capture','reversal'));
ALTER TABLE authorizations ADD COLUMN pending_op_amount bigint;
ALTER TABLE authorizations ADD COLUMN pending_op_at timestamptz;

-- Expiry failures are recorded and retried later instead of blocking the batch (review C2).
ALTER TABLE authorizations ADD COLUMN expiry_failed_at timestamptz;
ALTER TABLE authorizations ADD COLUMN expiry_error text;

ALTER TABLE refunds ADD COLUMN status text NOT NULL DEFAULT 'posted' CHECK (status IN ('pending','posted'));

CREATE INDEX authorizations_pending_idx ON authorizations (created_at) WHERE status = 'pending';
CREATE INDEX authorizations_pending_op_idx ON authorizations (pending_op_at) WHERE pending_op IS NOT NULL;
CREATE INDEX refunds_pending_idx ON refunds (created_at) WHERE status = 'pending';
