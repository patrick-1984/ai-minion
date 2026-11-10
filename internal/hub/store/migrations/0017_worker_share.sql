-- S4.15 privacy hardening: the share rule of each Worker, as the Worker last
-- declared it, kept durably. It is what lets the Hub decide whether it may
-- still show what it stored of a session (the pipe history) for a session the
-- Worker no longer lists: closed, offline or unshared. mode is one of all,
-- none, select, later; names are the selected sessions (a JSON array, only for
-- select); revision is the Worker's own monotonic revision of the rule.
-- A restriction is written before it is relied on, and a restart does not
-- lose it.
CREATE TABLE worker_share (
    worker_id  TEXT    NOT NULL PRIMARY KEY,
    mode       TEXT    NOT NULL CHECK (mode IN ('all', 'none', 'select', 'later')),
    names      TEXT    NOT NULL DEFAULT '[]',
    revision   INTEGER NOT NULL CHECK (revision >= 1),
    updated_at INTEGER NOT NULL
);
