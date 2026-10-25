-- Sprint 6 (ADR-021): recorded full chain verifications. The API verifies incrementally from the
-- last valid one (re-checking its anchor hash); the daily job and auditctl run full walks.
CREATE TABLE audit_verifications (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    ran_at            timestamptz NOT NULL DEFAULT now(),
    through_chain_seq bigint      NOT NULL,
    through_hash      char(64)    NOT NULL,
    checked           bigint      NOT NULL
);
CREATE INDEX audit_verifications_through_idx ON audit_verifications (through_chain_seq DESC);
