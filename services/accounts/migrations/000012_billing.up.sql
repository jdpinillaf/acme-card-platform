-- Sprint 4: per-account simulated clock, activation date, delinquency stage, business dates
-- persisted at reserve so a reconciler retry posts the same effective_at.
ALTER TABLE accounts
    ADD COLUMN clock_offset_seconds bigint NOT NULL DEFAULT 0 CHECK (clock_offset_seconds >= 0),
    ADD COLUMN activated_at timestamptz,
    ADD COLUMN delinquency text NOT NULL DEFAULT 'none' CHECK (delinquency IN ('none','past_due','charged_off'));
UPDATE accounts a SET activated_at = coalesce(
    (SELECT min(d.settled_at) FROM deposits d WHERE d.account_id = a.id AND d.status = 'settled'), a.updated_at)
    WHERE a.state = 'active';

ALTER TABLE payments
    ADD COLUMN received_at timestamptz,
    ADD COLUMN settle_effective_at timestamptz,
    ADD COLUMN return_effective_at timestamptz;
UPDATE payments SET received_at = created_at;
ALTER TABLE payments ALTER COLUMN received_at SET NOT NULL, ALTER COLUMN received_at SET DEFAULT now();

ALTER TABLE deposits ADD COLUMN effective_at timestamptz;
ALTER TABLE authorizations ADD COLUMN op_effective_at timestamptz;
ALTER TABLE refunds ADD COLUMN effective_at timestamptz;
