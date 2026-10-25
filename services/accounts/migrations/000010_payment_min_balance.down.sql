ALTER TABLE payments DROP CONSTRAINT payments_amount_minor_check;
ALTER TABLE payments ADD CONSTRAINT payments_amount_minor_check CHECK (amount_minor >= 100);
