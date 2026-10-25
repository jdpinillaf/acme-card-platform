ALTER TABLE accounts DROP COLUMN charge_off_pending_at, DROP COLUMN charge_off_effective_at, DROP COLUMN charge_off_amount;
ALTER TABLE accounts DROP CONSTRAINT accounts_restricted_reason_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_restricted_reason_check CHECK (restricted_reason IN ('payment_returned'));
