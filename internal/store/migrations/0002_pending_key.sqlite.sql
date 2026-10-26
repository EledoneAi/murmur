-- Laptops choose message ids: kept messages are keyed by their person and id, so one person's id never replaces
-- or drops what waits for another. The key leads with email, so it also serves Pending's lookup. SQLite can't
-- change a primary key in place: the table is rebuilt.
CREATE TABLE pending_new (
	id      TEXT NOT NULL,    -- the message's id
	msg     TEXT NOT NULL,    -- the message as JSON
	email   TEXT NOT NULL,
	device  TEXT NOT NULL,
	sender  TEXT NOT NULL,
	created BIGINT NOT NULL,
	expires BIGINT NOT NULL,
	PRIMARY KEY (email, id)
);
INSERT INTO pending_new (id, msg, email, device, sender, created, expires)
	SELECT id, msg, email, device, sender, created, expires FROM pending;
DROP TABLE pending;
ALTER TABLE pending_new RENAME TO pending;
CREATE INDEX pending_expires ON pending (expires);
