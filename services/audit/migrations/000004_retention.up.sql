-- Sprint 6 (ADR-021, I-7): archive-then-purge retention and auditor exports.

-- One row per archived chain range. The NDJSON and its signed manifest live in the Object-Lock
-- bucket; verified_at is set only after the archive was read back and re-hashed.
CREATE TABLE audit_archives (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    first_chain_seq bigint      NOT NULL,
    last_chain_seq  bigint      NOT NULL UNIQUE,
    first_prev_hash char(64)    NOT NULL,
    last_hash       char(64)    NOT NULL,
    count           bigint      NOT NULL,
    sha256          char(64)    NOT NULL,
    s3_key          text        NOT NULL,
    manifest_key    text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    verified_at     timestamptz,
    purged_at       timestamptz,
    CHECK (first_chain_seq >= 1 AND last_chain_seq >= first_chain_seq AND count = last_chain_seq - first_chain_seq + 1)
);

CREATE TABLE audit_exports (
    id         uuid        PRIMARY KEY,
    actor      text        NOT NULL,
    filter     jsonb       NOT NULL,
    count      bigint      NOT NULL,
    sha256     char(64)    NOT NULL,
    s3_key     text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Append-only, with two narrow exceptions: the one-off chain backfill (000002) and the purge of
-- rows a verified archive covers. Even with audit.purge set by hand, an unarchived row survives.
CREATE OR REPLACE FUNCTION audit_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    chain_cols text[] := ARRAY['chain_seq','prev_hash','hash'];
BEGIN
    IF TG_OP = 'UPDATE' AND current_setting('audit.backfill', true) = 'on' AND OLD.chain_seq IS NULL
       AND (to_jsonb(NEW) - chain_cols) = (to_jsonb(OLD) - chain_cols) THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' AND current_setting('audit.purge', true) = 'on' AND OLD.chain_seq IS NOT NULL
       AND EXISTS (SELECT 1 FROM audit_archives a WHERE OLD.chain_seq BETWEEN a.first_chain_seq AND a.last_chain_seq
                   AND a.verified_at IS NOT NULL) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit_events is append-only (% rejected)', TG_OP;
END;
$$;

-- The only way rows leave audit_events: every chain_seq up to `through` must be covered by
-- verified archives, contiguous from 1. Returns the number of rows removed.
CREATE FUNCTION audit_purge_archived(through bigint) RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE
    covered bigint;
    unverified bigint;
    n bigint;
BEGIN
    SELECT coalesce(sum(count), 0), count(*) FILTER (WHERE verified_at IS NULL)
      INTO covered, unverified
      FROM audit_archives WHERE last_chain_seq <= through;
    IF through < 1 OR unverified > 0 OR covered <> through
       OR NOT EXISTS (SELECT 1 FROM audit_archives WHERE last_chain_seq = through) THEN
        RAISE EXCEPTION 'audit_purge_archived(%): range not covered by verified archives', through;
    END IF;
    PERFORM set_config('audit.purge', 'on', true);
    DELETE FROM audit_events WHERE chain_seq <= through;
    GET DIAGNOSTICS n = ROW_COUNT;
    PERFORM set_config('audit.purge', 'off', true);
    UPDATE audit_archives SET purged_at = now() WHERE last_chain_seq <= through AND purged_at IS NULL;
    RETURN n;
END;
$$;
