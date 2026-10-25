-- Sprint 5 review fixes: one close-saga runner per account (lease, no connection held) and a
-- round-robin sweep cursor so parked closes never starve newer ones.
ALTER TABLE accounts
    ADD COLUMN close_lease_until timestamptz,
    ADD COLUMN close_lease_owner uuid,
    ADD COLUMN close_checked_at  timestamptz;
