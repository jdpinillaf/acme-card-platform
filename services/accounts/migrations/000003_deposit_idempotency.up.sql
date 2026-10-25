-- Second line of defence behind the gateway idempotency store: one deposit per request key.
ALTER TABLE deposits ADD COLUMN idempotency_key_hash char(64);
CREATE UNIQUE INDEX deposits_idempotency_key_hash_uq ON deposits (idempotency_key_hash) WHERE idempotency_key_hash IS NOT NULL;
