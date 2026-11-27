-- How the Hub updates a Worker (v0.24.0, made for the Android app): policy
-- auto makes the Hub send its own release to the Worker by itself, off makes
-- the Hub refuse worker_update for it. A Worker without a row is manual (the
-- Update button of an administrator only), as before, so nothing changes on
-- upgrade. updated_by is the administrator who set it: the Hub sends an
-- automatic update with that person's current rights.
--
-- The row goes with the Worker (removal) and is deleted when the Worker is
-- reassigned to another owner, like the other per-Worker control settings.
CREATE TABLE worker_app_update (
    worker_id  TEXT    PRIMARY KEY REFERENCES workers(id) ON DELETE CASCADE,
    policy     TEXT    NOT NULL CHECK (policy IN ('auto', 'off')),
    updated_by TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
) WITHOUT ROWID;
