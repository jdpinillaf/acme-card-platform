DROP INDEX authorizations_failed_unreleased_idx;
ALTER TABLE authorizations DROP COLUMN hold_released_at;
ALTER TABLE refunds DROP CONSTRAINT refunds_status_check;
ALTER TABLE refunds ADD CONSTRAINT refunds_status_check CHECK (status IN ('pending','posted'));
