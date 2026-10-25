-- Sprint 6: contiguous per-source publish numbers (pkg/outbox.SequenceMigration, ADR-021).
CREATE TABLE outbox_sequence (
    source   text   PRIMARY KEY,
    next_seq bigint NOT NULL CHECK (next_seq >= 1)
);
