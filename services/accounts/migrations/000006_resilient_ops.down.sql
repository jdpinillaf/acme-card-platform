DROP INDEX refunds_pending_idx;
DROP INDEX authorizations_pending_op_idx;
DROP INDEX authorizations_pending_idx;
ALTER TABLE refunds DROP COLUMN status;
ALTER TABLE authorizations DROP COLUMN expiry_error, DROP COLUMN expiry_failed_at, DROP COLUMN pending_op_at, DROP COLUMN pending_op_amount, DROP COLUMN pending_op;
ALTER TABLE authorizations DROP CONSTRAINT authorizations_status_check;
ALTER TABLE authorizations ADD CONSTRAINT authorizations_status_check CHECK (status IN ('approved','declined','captured','reversed','expired'));
