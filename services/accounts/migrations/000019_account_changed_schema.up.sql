-- The account.changed trigger must write to the outbox of its own schema whatever the session's
-- search_path (a reporting or maintenance session would otherwise misroute the event).
CREATE OR REPLACE FUNCTION accounts_emit_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    excluded text[] := ARRAY['destination_ciphertext','close_lease_owner','close_lease_until','close_checked_at','updated_at'];
    new_row jsonb := to_jsonb(NEW) - excluded;
    ev_id uuid := gen_random_uuid();
BEGIN
    IF TG_OP = 'UPDATE' AND new_row = (to_jsonb(OLD) - excluded) THEN
        RETURN NULL;
    END IF;
    EXECUTE format('INSERT INTO %I.outbox (event_id, payload) VALUES ($1, $2)', TG_TABLE_SCHEMA)
    USING ev_id, jsonb_build_object(
        'id', ev_id, 'type', 'account.changed', 'aggregate_type', 'account', 'aggregate_id', NEW.id::text,
        'actor', 'accounts', 'after', new_row || jsonb_build_object('updated_at', NEW.updated_at),
        'occurred_at', to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'));
    RETURN NULL;
END;
$$;
