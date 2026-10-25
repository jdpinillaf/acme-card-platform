DROP TABLE payout_attempts;
DROP TABLE payouts;
ALTER TABLE deposits DROP CONSTRAINT deposits_status_check;
ALTER TABLE deposits ADD CONSTRAINT deposits_status_check CHECK (status IN ('pending','settling','settled','failed'));
ALTER TABLE accounts DROP COLUMN close_stage, DROP COLUMN close_stage_at, DROP COLUMN close_requested_at, DROP COLUMN close_requested_by,
    DROP COLUMN close_reason, DROP COLUMN destination_ciphertext, DROP COLUMN destination_last4, DROP COLUMN final_due_at,
    DROP COLUMN close_apply_amount, DROP COLUMN close_apply_seq;
DROP INDEX accounts_open_customer_currency_uq;
ALTER TABLE accounts ADD CONSTRAINT accounts_customer_id_currency_key UNIQUE (customer_id, currency);
ALTER TABLE accounts DROP CONSTRAINT accounts_state_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_state_check CHECK (state IN ('pending_collateral','active','closed'));
