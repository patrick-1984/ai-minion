-- Minion control (v0.9.0, A17): master sessions and two-sided consent.
--
-- A session controls sessions on other Workers only when it is nominated as a
-- master session on its Worker (master_session) and the target Worker accepts
-- control from that Worker (control_accept, or accept_all in worker_control).
-- Every session is a slave session by default and no Worker accepts control
-- by default. worker_control.user_managed (default 1) lets people and API keys
-- control the Worker's sessions; 0 leaves them to master sessions only.
--
-- The v0.8 session-control grants are migrated: each becomes a master session
-- whose legacy_targets_json keeps its exact old targets as an extra limit, and
-- every target Worker accepts control from the controller's Worker, so that no
-- grant reaches more or less than before. Like session_control, the rows have
-- no foreign keys: a Worker's removal or reassignment deletes them in its own
-- transaction.
CREATE TABLE worker_control (
    worker_id    TEXT    PRIMARY KEY CHECK (worker_id <> ''),
    user_managed INTEGER NOT NULL DEFAULT 1 CHECK (user_managed IN (0, 1)),
    accept_all   INTEGER NOT NULL DEFAULT 0 CHECK (accept_all IN (0, 1))
);

CREATE TABLE master_session (
    worker_id           TEXT    NOT NULL,
    session_name        TEXT    NOT NULL,
    legacy_targets_json TEXT,
    created_by          TEXT    NOT NULL,
    created_at          INTEGER NOT NULL,
    PRIMARY KEY (worker_id, session_name),
    CHECK (worker_id <> '' AND session_name <> '')
);

CREATE TABLE control_accept (
    target_worker_id TEXT    NOT NULL,
    source_worker_id TEXT    NOT NULL,
    sessions_json    TEXT,
    created_by       TEXT    NOT NULL,
    created_at       INTEGER NOT NULL,
    PRIMARY KEY (target_worker_id, source_worker_id),
    CHECK (target_worker_id <> '' AND source_worker_id <> '' AND target_worker_id <> source_worker_id)
);

INSERT INTO master_session (worker_id, session_name, legacy_targets_json, created_by, created_at)
SELECT worker_id, session_name, targets_json, created_by, created_at FROM session_control;

-- Targets that name a Worker or one of its sessions: that Worker accepts the controller's Worker.
INSERT OR IGNORE INTO control_accept (target_worker_id, source_worker_id, sessions_json, created_by, created_at)
SELECT json_extract(t.value, '$.worker_id'), sc.worker_id, NULL, sc.created_by, sc.created_at
FROM session_control sc, json_each(sc.targets_json) t
WHERE json_extract(t.value, '$.kind') IN ('worker', 'session')
  AND json_extract(t.value, '$.worker_id') IS NOT NULL
  AND json_extract(t.value, '$.worker_id') <> sc.worker_id;

-- "everything": every other Worker accepts the controller's Worker.
INSERT OR IGNORE INTO control_accept (target_worker_id, source_worker_id, sessions_json, created_by, created_at)
SELECT w.id, sc.worker_id, NULL, sc.created_by, sc.created_at
FROM session_control sc, json_each(sc.targets_json) t, workers w
WHERE json_extract(t.value, '$.kind') = 'everything' AND w.id <> sc.worker_id;
