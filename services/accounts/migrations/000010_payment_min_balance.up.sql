-- A remaining debt under 1.00 must be payable (review I3): the minimum is enforced in code as
-- min(100, outstanding), so the table only requires a positive amount.
ALTER TABLE payments DROP CONSTRAINT payments_amount_minor_check;
ALTER TABLE payments ADD CONSTRAINT payments_amount_minor_check CHECK (amount_minor > 0);
