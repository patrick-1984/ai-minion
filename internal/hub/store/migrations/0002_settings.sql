-- Story 3.4: Hub settings edited in the panel (AD-19). One row per setting
-- key. A plain setting keeps its text in value; a secret setting keeps the
-- sealed (AES-256-GCM, bound to settings/value_enc/<key>) bytes in value_enc.
-- Exactly one of the two columns is set.
CREATE TABLE settings (
    key        TEXT    NOT NULL PRIMARY KEY,
    value      TEXT,
    value_enc  BLOB,
    updated_at INTEGER NOT NULL,
    updated_by TEXT    NOT NULL DEFAULT '',
    CHECK ((value IS NULL) <> (value_enc IS NULL))
);
