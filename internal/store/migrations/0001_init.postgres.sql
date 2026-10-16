-- Times are microseconds since the Unix epoch, as in SQLite, so both run the same queries.

CREATE TABLE users (
	email    TEXT PRIMARY KEY,
	handle   TEXT NOT NULL,
	name     TEXT NOT NULL,
	admin    BOOLEAN NOT NULL,
	disabled BOOLEAN NOT NULL,
	created  BIGINT NOT NULL
);

CREATE TABLE devices (
	id         TEXT PRIMARY KEY,
	email      TEXT NOT NULL,
	host       TEXT NOT NULL,
	token_hash TEXT NOT NULL,
	created    BIGINT NOT NULL,
	last_seen  BIGINT NOT NULL,
	revoked    BOOLEAN NOT NULL,
	version    TEXT NOT NULL,
	report     BYTEA,
	report_at  BIGINT NOT NULL
);

CREATE TABLE pending (
	id      TEXT PRIMARY KEY, -- the message's id
	msg     TEXT NOT NULL,    -- the message as JSON
	email   TEXT NOT NULL,
	device  TEXT NOT NULL,
	sender  TEXT NOT NULL,
	created BIGINT NOT NULL,
	expires BIGINT NOT NULL
);
CREATE INDEX pending_email ON pending (email);
CREATE INDEX pending_expires ON pending (expires);

CREATE TABLE codes (
	id        TEXT PRIMARY KEY,
	email     TEXT NOT NULL,
	host      TEXT NOT NULL,
	challenge TEXT NOT NULL,
	expires   BIGINT NOT NULL
);

CREATE TABLE kv (
	key   TEXT PRIMARY KEY,
	value BYTEA
);

CREATE TABLE events (
	id      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	at      BIGINT NOT NULL,
	kind    TEXT NOT NULL,
	actor   TEXT NOT NULL,
	subject TEXT NOT NULL,
	detail  TEXT NOT NULL
);
CREATE INDEX events_at ON events (at);

CREATE TABLE leases (
	name    TEXT PRIMARY KEY,
	holder  TEXT NOT NULL,
	expires BIGINT NOT NULL
);
