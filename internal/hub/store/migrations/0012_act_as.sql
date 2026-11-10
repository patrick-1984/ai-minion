-- Story 6.5: act-as (AD-10, R45). An admin's browser session can enter another
-- user's space for a limited time. The state belongs to the browser session:
-- logging out, expiry of the session or a credential change deletes the
-- session and, through the cascade, the act-as row with it. Time columns are
-- unix milliseconds.
CREATE TABLE act_as_sessions (
    session_id TEXT    NOT NULL PRIMARY KEY REFERENCES sessions (id) ON DELETE CASCADE,
    actor_id   TEXT    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    target_id  TEXT    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    started_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    CHECK (actor_id <> target_id),
    CHECK (expires_at > started_at)
);

CREATE INDEX act_as_sessions_expires ON act_as_sessions (expires_at);
CREATE INDEX act_as_sessions_target  ON act_as_sessions (target_id);
