-- Result cache and local audit of the Worker op executor (AD-15, AD-16, AD-20).
--
-- op_results answers a duplicate request_id from the cached result for ten
-- minutes (no layer retries a write). It holds the WriteResult JSON (outcome,
-- reason, flags, warnings and pane identity), never terminal content or the
-- text that was sent.
CREATE TABLE op_results (
    request_id   TEXT    PRIMARY KEY CHECK (length(request_id) > 0),
    op           TEXT    NOT NULL,
    session_name TEXT    NOT NULL,
    force        INTEGER NOT NULL CHECK (force IN (0, 1)),
    result       TEXT    NOT NULL,
    created_at   INTEGER NOT NULL
);

CREATE INDEX op_results_created_at ON op_results (created_at);

-- local_audit has one row per write op the executor handled: what was done to
-- which target and how it ended, never the content (AD-20).
CREATE TABLE local_audit (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ts         INTEGER NOT NULL,
    op         TEXT    NOT NULL,
    target     TEXT    NOT NULL,
    outcome    TEXT    NOT NULL,
    reason     TEXT    NOT NULL,
    forced     INTEGER NOT NULL CHECK (forced IN (0, 1)),
    request_id TEXT    NOT NULL
);
