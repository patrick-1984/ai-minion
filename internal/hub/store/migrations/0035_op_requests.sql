-- Request de-duplication of Hub-scope write operations: the answer of a
-- request_id, kept for ten minutes, so that a repeated request is answered
-- from here instead of running again. args_sha256 is the hash of the decoded
-- arguments; no row holds a secret (only operations without one are stored).
CREATE TABLE op_requests (
    request_id  TEXT PRIMARY KEY,
    op          TEXT NOT NULL,
    actor       TEXT NOT NULL,
    args_sha256 TEXT NOT NULL,
    result_json TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);
CREATE INDEX op_requests_created ON op_requests (created_at);
