-- Append-only audit log. UPDATE, DELETE and TRUNCATE are rejected by triggers, so even the owner
-- role cannot rewrite history. In AWS, the app role additionally lacks UPDATE/DELETE grants.
CREATE TABLE audit_events (
    id                   uuid        PRIMARY KEY,
    event_type           text        NOT NULL,
    aggregate_type       text        NOT NULL,
    aggregate_id         text        NOT NULL,
    actor                text,
    idempotency_key_hash char(64),
    http_status          integer,
    before               jsonb,
    after                jsonb,
    occurred_at          timestamptz NOT NULL,
    recorded_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_aggregate_idx ON audit_events (aggregate_type, aggregate_id, occurred_at);
CREATE INDEX audit_events_type_idx ON audit_events (event_type, occurred_at);

CREATE FUNCTION audit_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only (% rejected)', TG_OP;
END;
$$;

CREATE TRIGGER audit_events_no_update_delete
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_append_only();

CREATE TRIGGER audit_events_no_truncate
    BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only();
