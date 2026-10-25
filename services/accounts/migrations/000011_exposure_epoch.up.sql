-- Incremented by every payment return in its finalize transaction (review M1): a restriction lift
-- decided on an older exposure reading must not clear a newer restriction.
ALTER TABLE accounts ADD COLUMN exposure_epoch bigint NOT NULL DEFAULT 0;
