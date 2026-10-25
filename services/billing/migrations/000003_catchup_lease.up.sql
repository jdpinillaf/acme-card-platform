-- Catch-up lock as a lease (no connection held while catching up; see ADR-019).
ALTER TABLE billing_accounts ADD COLUMN lease_until timestamptz, ADD COLUMN lease_owner uuid;
