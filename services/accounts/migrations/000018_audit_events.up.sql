-- Sprint 6 (ADR-021): every change to an account row emits account.changed with the full row, so
-- audit replay sees every mutation (no call site can forget it). Destination ciphertext and the
-- close-saga lease bookkeeping are never included, and lease-only changes emit nothing.
CREATE FUNCTION accounts_emit_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    excluded text[] := ARRAY['destination_ciphertext','close_lease_owner','close_lease_until','close_checked_at','updated_at'];
    new_row jsonb := to_jsonb(NEW) - excluded;
    ev_id uuid := gen_random_uuid();
BEGIN
    IF TG_OP = 'UPDATE' AND new_row = (to_jsonb(OLD) - excluded) THEN
        RETURN NULL;
    END IF;
    INSERT INTO outbox (event_id, payload) VALUES (ev_id, jsonb_build_object(
        'id', ev_id, 'type', 'account.changed', 'aggregate_type', 'account', 'aggregate_id', NEW.id::text,
        'actor', 'accounts', 'after', new_row || jsonb_build_object('updated_at', NEW.updated_at),
        'occurred_at', to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')));
    RETURN NULL;
END;
$$;
CREATE TRIGGER accounts_changed AFTER INSERT OR UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION accounts_emit_changed();

-- One snapshot run per UTC day.
CREATE TABLE snapshot_runs (
    day        date        PRIMARY KEY,
    started_at timestamptz NOT NULL DEFAULT now()
);
