CREATE OR REPLACE FUNCTION audit_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only (% rejected)', TG_OP;
END;
$$;
DROP TABLE chain_head;
DROP INDEX audit_events_source_idx;
ALTER TABLE audit_events DROP COLUMN source, DROP COLUMN source_seq, DROP COLUMN chain_seq, DROP COLUMN prev_hash, DROP COLUMN hash, DROP COLUMN redacted;
