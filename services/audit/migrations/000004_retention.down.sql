DROP FUNCTION IF EXISTS audit_purge_archived(bigint);
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
DROP TABLE IF EXISTS audit_exports;
DROP TABLE IF EXISTS audit_archives;
