-- Story 3.5: local accounts and browser sessions (AD-10, AD-16). The auth
-- domain owns these tables.

-- One row per account. password_hash is a PHC-format Argon2id string, empty
-- for an account that has no local password. Time columns are unix
-- milliseconds. credential_version is bumped by a password change; a session
-- is valid only while it carries the current version.
CREATE TABLE users (
    id                   TEXT    NOT NULL PRIMARY KEY,                -- UUIDv7
    username             TEXT    NOT NULL COLLATE NOCASE UNIQUE,
    email                TEXT    NOT NULL DEFAULT '' COLLATE NOCASE,
    password_hash        TEXT    NOT NULL DEFAULT '',
    must_change_password INTEGER NOT NULL DEFAULT 0 CHECK (must_change_password IN (0, 1)),
    credential_version   INTEGER NOT NULL DEFAULT 1,
    active               INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
);

-- An e-mail address identifies at most one account; an empty one is no address.
CREATE UNIQUE INDEX users_email ON users (email) WHERE email <> '';

-- Granted roles. Every user is at least a "user"; an "admin" row makes the
-- effective role admin. IdP sync replaces only the rows with source 'idp'.
CREATE TABLE user_roles (
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role    TEXT NOT NULL CHECK (role IN ('admin', 'user')),
    source  TEXT NOT NULL CHECK (source IN ('local', 'idp')),
    PRIMARY KEY (user_id, role, source)
);

-- Markers of one-time actions, for example 'admin_seed'. A marker is written
-- in the transaction of the action it records.
CREATE TABLE auth_meta (
    key        TEXT    NOT NULL PRIMARY KEY,
    value      TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);

-- Browser sessions. Only the SHA-256 (hex) of the 32-byte cookie value is
-- stored, never the cookie itself.
CREATE TABLE sessions (
    id                 TEXT    NOT NULL PRIMARY KEY,                  -- UUIDv7
    token_hash         TEXT    NOT NULL UNIQUE,
    user_id            TEXT    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    credential_version INTEGER NOT NULL,
    created_at         INTEGER NOT NULL,
    expires_at         INTEGER NOT NULL
);

CREATE INDEX sessions_user    ON sessions (user_id);
CREATE INDEX sessions_expires ON sessions (expires_at);
