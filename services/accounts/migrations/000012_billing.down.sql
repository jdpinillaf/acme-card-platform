ALTER TABLE refunds DROP COLUMN effective_at;
ALTER TABLE authorizations DROP COLUMN op_effective_at;
ALTER TABLE deposits DROP COLUMN effective_at;
ALTER TABLE payments DROP COLUMN received_at, DROP COLUMN settle_effective_at, DROP COLUMN return_effective_at;
ALTER TABLE accounts DROP COLUMN clock_offset_seconds, DROP COLUMN activated_at, DROP COLUMN delinquency;
