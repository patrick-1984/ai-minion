-- Story 6.1: API keys and their scopes (AD-11, AD-16). The apikey domain owns
-- these tables. Time columns are unix milliseconds.

-- One row per key. Only the SHA-256 of the key text is stored (hash), never the
-- key; hint is the first 8 characters after "mn_", safe to display. actions is
-- the sorted, comma-separated list of the actions the key holds on every
-- resource of its scope. expires_at and revoked_at are NULL while unset.
-- bound_worker_id binds the key to a Worker identity (call streams only); it
-- has no foreign key because the workers table belongs to another domain, and
-- a removed Worker leaves the binding in place, which stays harmless because
-- no call stream of that Worker exists any more.
CREATE TABLE api_keys (
    id              TEXT    NOT NULL PRIMARY KEY,                     -- UUIDv7
    owner_id        TEXT    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name            TEXT    NOT NULL,
    hash            BLOB    NOT NULL UNIQUE CHECK (length(hash) = 32),
    hint            TEXT    NOT NULL CHECK (length(hint) = 8),
    actions         TEXT    NOT NULL CHECK (actions <> ''),
    bound_worker_id TEXT,
    created_at      INTEGER NOT NULL,
    expires_at      INTEGER,
    revoked_at      INTEGER
);

CREATE INDEX api_keys_owner ON api_keys (owner_id);
CREATE INDEX api_keys_bound ON api_keys (bound_worker_id) WHERE bound_worker_id IS NOT NULL;

-- The resources of a key scope, one row each. The members that do not belong
-- to the kind are empty strings, so that the primary key stays unique.
CREATE TABLE api_key_scopes (
    key_id       TEXT NOT NULL REFERENCES api_keys (id) ON DELETE CASCADE,
    kind         TEXT NOT NULL CHECK (kind IN ('everything', 'user', 'worker', 'session')),
    user_id      TEXT NOT NULL DEFAULT '',
    worker_id    TEXT NOT NULL DEFAULT '',
    session_name TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (key_id, kind, user_id, worker_id, session_name),
    CHECK (
        (kind = 'everything' AND user_id = '' AND worker_id = '' AND session_name = '') OR
        (kind = 'user'       AND user_id <> '' AND worker_id = '' AND session_name = '') OR
        (kind = 'worker'     AND user_id = '' AND worker_id <> '' AND session_name = '') OR
        (kind = 'session'    AND user_id = '' AND worker_id <> '' AND session_name <> '')
    )
);
