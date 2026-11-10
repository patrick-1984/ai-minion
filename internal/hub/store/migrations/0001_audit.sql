-- Audit log (AD-20, Story 3.3). One row per security-relevant action: who
-- (actor, acting_as), what (action, target) and how it ended (outcome). It
-- never holds content: the audit package rejects values longer than 256
-- characters or with control characters before they get here.
CREATE TABLE audit_events (
    id        TEXT    PRIMARY KEY,           -- UUIDv7
    ts        INTEGER NOT NULL,              -- unix milliseconds
    actor     TEXT    NOT NULL,
    acting_as TEXT    NOT NULL DEFAULT '',   -- the user an admin acts as, '' otherwise
    action    TEXT    NOT NULL,
    target    TEXT    NOT NULL DEFAULT '',
    outcome   TEXT    NOT NULL
);

-- Newest-first listing and its keyset cursor (ts, id).
CREATE INDEX audit_events_ts ON audit_events (ts DESC, id DESC);
CREATE INDEX audit_events_actor_ts ON audit_events (actor, ts DESC, id DESC);
CREATE INDEX audit_events_action_ts ON audit_events (action, ts DESC, id DESC);
