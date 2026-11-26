-- Hidden sessions per Worker (v0.23.0). Before, only the account-wide
-- preference user_prefs.allow_hidden_sessions of the caller decided whether a
-- new hidden session was allowed (while the Hub setting hidden_sessions is
-- user-controlled). Now each Worker can carry its own choice, set by its owner
-- or an administrator: allow 1 allows new hidden sessions on that Worker, 0
-- refuses them. A Worker without a row keeps the old behaviour: the caller's
-- account-wide preference decides, so nothing changes on upgrade.
--
-- The row goes with the Worker (removal) and is deleted when the Worker is
-- reassigned to another owner, like the other per-Worker control settings.
CREATE TABLE worker_hidden_sessions (
    worker_id  TEXT    PRIMARY KEY REFERENCES workers(id) ON DELETE CASCADE,
    allow      INTEGER NOT NULL CHECK (allow IN (0, 1)),
    updated_by TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
) WITHOUT ROWID;
