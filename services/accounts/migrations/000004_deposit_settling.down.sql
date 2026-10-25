ALTER TABLE deposits DROP CONSTRAINT deposits_status_check;
ALTER TABLE deposits ADD CONSTRAINT deposits_status_check CHECK (status IN ('pending','settled','failed'));
