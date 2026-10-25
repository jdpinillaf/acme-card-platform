DROP INDEX accounts_restricted_idx;
ALTER TABLE accounts DROP COLUMN restricted_at, DROP COLUMN restricted_reason;
DROP TABLE payments;
