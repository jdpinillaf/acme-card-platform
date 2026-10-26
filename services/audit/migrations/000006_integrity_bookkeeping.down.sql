DROP TRIGGER IF EXISTS audit_verifications_no_truncate ON audit_verifications;
DROP TRIGGER IF EXISTS audit_verifications_no_update_delete ON audit_verifications;
DROP FUNCTION IF EXISTS audit_verifications_append_only();
DROP TRIGGER IF EXISTS audit_archives_no_truncate ON audit_archives;
DROP TRIGGER IF EXISTS audit_archives_guard ON audit_archives;
DROP FUNCTION IF EXISTS audit_archives_guard();
ALTER TABLE audit_verifications DROP COLUMN IF EXISTS signature;
ALTER TABLE audit_archives DROP COLUMN IF EXISTS source_floors;
