-- Laptops choose message ids: kept messages are keyed by their person and id, so one person's id never replaces
-- or drops what waits for another. The key leads with email, so it also serves Pending's lookup.
ALTER TABLE pending DROP CONSTRAINT pending_pkey;
ALTER TABLE pending ADD PRIMARY KEY (email, id);
DROP INDEX pending_email;
