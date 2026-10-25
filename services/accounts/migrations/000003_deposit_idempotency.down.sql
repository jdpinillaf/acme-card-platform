DROP INDEX deposits_idempotency_key_hash_uq;
ALTER TABLE deposits DROP COLUMN idempotency_key_hash;
