-- Reserved operations the ledger refuses permanently end in a terminal state instead of being
-- retried forever (audit H2); refunds gain 'failed'.
ALTER TABLE refunds DROP CONSTRAINT refunds_status_check;
ALTER TABLE refunds ADD CONSTRAINT refunds_status_check CHECK (status IN ('pending','posted','failed'));

-- The reconciler fails a stale pending authorization first and releases its hold second (audit
-- H3). hold_released_at records the second step so a crash in between is retried.
ALTER TABLE authorizations ADD COLUMN hold_released_at timestamptz;
CREATE INDEX authorizations_failed_unreleased_idx ON authorizations (created_at) WHERE status = 'failed' AND hold_released_at IS NULL;
