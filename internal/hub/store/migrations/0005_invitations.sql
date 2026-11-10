-- Story 4.5: invitation keys (AD-7, AD-16). Only the workers domain package
-- writes this table.

-- One row per invitation key created in the Hub. The key itself
-- (mnk_<fingerprint>.<secret>) is shown once and never stored: only the
-- SHA-256 of the 32 secret bytes is. Time columns are unix milliseconds.
CREATE TABLE invitation_keys (
    id          TEXT    PRIMARY KEY,                -- UUIDv7
    secret_hash BLOB    NOT NULL UNIQUE CHECK (length(secret_hash) = 32),
    name        TEXT    NOT NULL DEFAULT '',
    mode        TEXT    NOT NULL CHECK (mode IN ('one_time', 'reusable')),
    kind        TEXT    NOT NULL CHECK (kind IN ('private', 'infrastructure')),
    ttl         TEXT    NOT NULL CHECK (ttl IN ('1h', '4h', '8h', '24h', '1w', '1m', 'unlimited')),
    creator_id  TEXT    NOT NULL,                   -- the owner of the Workers it enrolls
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER,                            -- NULL for ttl 'unlimited'
    uses        INTEGER NOT NULL DEFAULT 0 CHECK (uses >= 0),
    max_uses    INTEGER CHECK (max_uses IS NULL OR max_uses >= 1), -- 1 for one_time, NULL for reusable
    revoked     INTEGER NOT NULL DEFAULT 0 CHECK (revoked IN (0, 1)),
    revoked_at  INTEGER
);

CREATE INDEX invitation_keys_creator ON invitation_keys (creator_id, created_at);
