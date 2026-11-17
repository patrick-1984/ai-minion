-- v0.10.0: per-user preferences, changed by each user on their own account
-- page (account_prefs_set). One row per user that changed a preference; a user
-- without a row has the defaults. allow_hidden_sessions lets the user create
-- sessions that they could not see or join from their own terminal (Windows
-- ConPTY inside the Worker, a tmux server that is not their default one) when
-- the Hub setting hidden_sessions is user-controlled; off by default.
CREATE TABLE user_prefs (
    user_id               TEXT    NOT NULL PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    allow_hidden_sessions INTEGER NOT NULL DEFAULT 0 CHECK (allow_hidden_sessions IN (0, 1)),
    updated_at            INTEGER NOT NULL           -- unix milliseconds
);
