-- Sprint 6 (ADR-021): tamper-evident hash chain and per-source numbering.
ALTER TABLE audit_events
    ADD COLUMN source     text,
    ADD COLUMN source_seq bigint,
    ADD COLUMN chain_seq  bigint UNIQUE,
    ADD COLUMN prev_hash  char(64),
    ADD COLUMN hash       char(64),
    ADD COLUMN redacted   boolean NOT NULL DEFAULT false;
CREATE INDEX audit_events_source_idx ON audit_events (source, source_seq) WHERE source IS NOT NULL;

-- The single chain head every insert locks: chain_seq stays contiguous.
CREATE TABLE chain_head (
    id        int      PRIMARY KEY CHECK (id = 1),
    chain_seq bigint   NOT NULL,
    hash      char(64) NOT NULL
);
INSERT INTO chain_head VALUES (1, 0, repeat('0', 64));

-- Append-only, with one narrow exception: the one-off backfill may set the chain columns of rows
-- written before chaining existed (nothing else in the row may change).
CREATE OR REPLACE FUNCTION audit_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    chain_cols text[] := ARRAY['chain_seq','prev_hash','hash'];
BEGIN
    IF TG_OP = 'UPDATE' AND current_setting('audit.backfill', true) = 'on' AND OLD.chain_seq IS NULL
       AND (to_jsonb(NEW) - chain_cols) = (to_jsonb(OLD) - chain_cols) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'audit_events is append-only (% rejected)', TG_OP;
END;
$$;
