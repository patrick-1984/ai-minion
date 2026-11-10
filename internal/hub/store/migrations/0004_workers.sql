-- Story 4.3: the Hub identity record, enrolled Workers and tombstones (AD-5,
-- AD-9, AD-16, AD-17). Only the workers domain package writes these tables.

-- The Hub's identity key lives in <data>/identity.key. This single row records
-- which public key that file must hold, so that a missing or replaced key file
-- is detected at start instead of silently creating a new identity (a new
-- identity would strand every enrolled Worker).
CREATE TABLE hub_identity (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    pub         BLOB    NOT NULL CHECK (length(pub) = 32),
    fingerprint TEXT    NOT NULL,
    created_at  INTEGER NOT NULL             -- unix milliseconds
);

-- One row per enrolled Worker. worker_pub is UNIQUE (a repeated enrollment of
-- the same key returns the existing row); names are unique per owner (AD-17).
-- owner_id has no foreign key: the users table belongs to the auth domain.
CREATE TABLE workers (
    id         TEXT    PRIMARY KEY,          -- UUIDv7
    worker_pub BLOB    NOT NULL UNIQUE CHECK (length(worker_pub) = 32),
    name       TEXT    NOT NULL,
    owner_id   TEXT    NOT NULL,
    kind       TEXT    NOT NULL CHECK (kind IN ('private', 'infrastructure')),
    created_at INTEGER NOT NULL,             -- unix milliseconds
    UNIQUE (owner_id, name)
);

CREATE INDEX workers_owner ON workers (owner_id);

-- Removing a Worker replaces its row by a tombstone that keeps the public key,
-- so that a later connect from that key is answered with a signed revoke
-- (AD-9). A tombstone is never deleted by time.
CREATE TABLE worker_tombstones (
    worker_pub BLOB    PRIMARY KEY CHECK (length(worker_pub) = 32),
    worker_id  TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    owner_id   TEXT    NOT NULL,
    kind       TEXT    NOT NULL CHECK (kind IN ('private', 'infrastructure')),
    created_at INTEGER NOT NULL,             -- when the Worker was enrolled
    removed_at INTEGER NOT NULL,             -- unix milliseconds
    removed_by TEXT    NOT NULL DEFAULT ''
);
