-- Sprint 5: the off-cycle final statement cut at close (ADR-020).
ALTER TABLE statements ADD COLUMN final boolean NOT NULL DEFAULT false;
