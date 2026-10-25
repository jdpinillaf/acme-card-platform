-- Claims no longer hold a transaction (and a pool connection) open while the request runs. A claim
-- is a committed in_progress row with a lease; duplicates poll until it completes, and a row whose
-- lease expired (owner died) is taken over. owner guards Complete/Abort against stale owners.
ALTER TABLE idempotency_keys ADD COLUMN lease_until timestamptz;
ALTER TABLE idempotency_keys ADD COLUMN owner uuid;
