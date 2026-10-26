-- Sprint 6 review fixes (ADR-021): the bookkeeping that verify and purge trust is append-only too.

-- Per-source highest source_seq inside an archive: gap detection starts after purged ranges.
ALTER TABLE audit_archives ADD COLUMN source_floors jsonb NOT NULL DEFAULT '{}';
-- Full-walk anchors are signed with the audit key; an unsigned or forged row is ignored.
ALTER TABLE audit_verifications ADD COLUMN signature text NOT NULL DEFAULT '';

-- audit_archives: inserted unverified and unpurged; afterwards only verified_at and purged_at may
-- be set, once each. No delete.
CREATE FUNCTION audit_archives_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    marks text[] := ARRAY['verified_at','purged_at'];
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.verified_at IS NOT NULL OR NEW.purged_at IS NOT NULL THEN
            RAISE EXCEPTION 'audit_archives: a new archive is neither verified nor purged';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND (to_jsonb(NEW) - marks) = (to_jsonb(OLD) - marks)
       AND (OLD.verified_at IS NULL OR NEW.verified_at IS NOT DISTINCT FROM OLD.verified_at)
       AND (OLD.purged_at IS NULL OR NEW.purged_at IS NOT DISTINCT FROM OLD.purged_at)
       AND (NEW.purged_at IS NULL OR NEW.verified_at IS NOT NULL) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'audit_archives is append-only (% rejected)', TG_OP;
END;
$$;
CREATE TRIGGER audit_archives_guard BEFORE INSERT OR UPDATE OR DELETE ON audit_archives
    FOR EACH ROW EXECUTE FUNCTION audit_archives_guard();
CREATE TRIGGER audit_archives_no_truncate BEFORE TRUNCATE ON audit_archives
    FOR EACH STATEMENT EXECUTE FUNCTION audit_archives_guard();

CREATE FUNCTION audit_verifications_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_verifications is append-only (% rejected)', TG_OP;
END;
$$;
CREATE TRIGGER audit_verifications_no_update_delete BEFORE UPDATE OR DELETE ON audit_verifications
    FOR EACH ROW EXECUTE FUNCTION audit_verifications_append_only();
CREATE TRIGGER audit_verifications_no_truncate BEFORE TRUNCATE ON audit_verifications
    FOR EACH STATEMENT EXECUTE FUNCTION audit_verifications_append_only();
