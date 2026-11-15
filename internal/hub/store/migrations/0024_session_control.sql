-- Session control (v0.8): an admin lets one controller session reach other
-- Workers and sessions. One row per controller session (worker_id,
-- session_name). targets_json is a JSON array in the shape of an API-key
-- scope: one {"kind":"everything"} resource alone, or a list of "worker" and
-- "session" resources. There are no seeded rows: without a row a session holds
-- no grant. worker_id has no foreign key, like trust: every transaction that
-- reassigns or removes a Worker deletes the rows of that controller Worker
-- itself. Target ids inside targets_json are not cleaned up; a stale id
-- matches nothing.
CREATE TABLE session_control (
    worker_id    TEXT    NOT NULL,
    session_name TEXT    NOT NULL,
    targets_json TEXT    NOT NULL,
    created_by   TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,           -- unix milliseconds
    PRIMARY KEY (worker_id, session_name),
    CHECK (worker_id <> '' AND session_name <> '')
);
