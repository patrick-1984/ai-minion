-- File transfer (files.v1, v0.24.0, BRD minion-file-transfer 5.6, 5.9, 5.11).
--
-- 1. Grant and acceptance flags. files_pull lets the source list and pull
--    files of the target session, files_push lets it push files into it.
--    Every existing row keeps 0: a grant or an acceptance stored before
--    v0.24 has no file access. On a session grant both flags need a verified
--    source (source_incarnation <> ''; a claimed source never carries file
--    authority) and files_push needs level Control. On an acceptance a flag
--    applies only through its own row (never through accept_all).
ALTER TABLE session_grant ADD COLUMN files_pull INTEGER NOT NULL DEFAULT 0
    CHECK (files_pull = 0 OR (files_pull = 1 AND source_incarnation <> ''));
ALTER TABLE session_grant ADD COLUMN files_push INTEGER NOT NULL DEFAULT 0
    CHECK (files_push = 0 OR (files_push = 1 AND level = 3 AND source_incarnation <> ''));

ALTER TABLE control_accept ADD COLUMN files_pull INTEGER NOT NULL DEFAULT 0 CHECK (files_pull IN (0, 1));
ALTER TABLE control_accept ADD COLUMN files_push INTEGER NOT NULL DEFAULT 0 CHECK (files_push IN (0, 1));

-- 2. The Hub's per-Worker override of file transfer (on, send_only,
--    receive_only, off), sent with the enrollment policy like preview_only.
--    Existing Workers read as on: the Worker's local switch and the Hub-wide
--    policy still decide.
ALTER TABLE workers ADD COLUMN file_transfer TEXT NOT NULL DEFAULT 'on'
    CHECK (file_transfer IN ('on', 'send_only', 'receive_only', 'off'));

-- 3. API keys: no change. A key holds a file action only when its stored
--    actions name file_read or file_write; a key stored before v0.24 names
--    neither.

-- 4. The durable record of every transfer attempt (denials included), written
--    before any byte moves (state admitted or denied), owner-scoped in
--    file_transfers_list and kept for file_transfer_retention_days. It holds
--    no absolute path, no token and no content: roots are display roots,
--    path is relative to the target root, receipt_hash is the SHA-256 of the
--    receiving leg's token (hex), never the token. The rows outlive the
--    Workers they name (no foreign keys): the owners at admission keep their
--    history. Times are unix milliseconds.
CREATE TABLE file_transfers (
    id                TEXT    PRIMARY KEY CHECK (id <> ''),          -- UUIDv7
    direction         TEXT    NOT NULL CHECK (direction IN ('pull', 'push')),
    state             TEXT    NOT NULL CHECK (state IN ('admitted', 'prepared', 'delivered', 'partial',
                                                        'failed_not_sent', 'served', 'unknown', 'denied')),
    reason            TEXT    NOT NULL DEFAULT '',
    principal         TEXT    NOT NULL,                              -- display form (user:..., key:..., worker:...)
    principal_user_id TEXT    NOT NULL DEFAULT '',                   -- the user whose history it is
    acting_as         TEXT    NOT NULL DEFAULT '',
    source_worker_id  TEXT    NOT NULL DEFAULT '',                   -- '' for a REST or browser caller
    source_session    TEXT    NOT NULL DEFAULT '',
    source_owner_id   TEXT    NOT NULL DEFAULT '',
    target_worker_id  TEXT    NOT NULL CHECK (target_worker_id <> ''),
    target_session    TEXT    NOT NULL,
    target_owner_id   TEXT    NOT NULL DEFAULT '',
    grant_ids         TEXT    NOT NULL DEFAULT '',                   -- JSON array of session_grant ids, or ''
    acceptance_id     TEXT    NOT NULL DEFAULT '',
    policy            TEXT    NOT NULL DEFAULT '',                   -- policy snapshot (audit grammar)
    caller_root       TEXT    NOT NULL DEFAULT '',
    target_root       TEXT    NOT NULL DEFAULT '',
    path              TEXT    NOT NULL DEFAULT '',
    kind              TEXT    NOT NULL DEFAULT '' CHECK (kind IN ('', 'file', 'dir')),
    files             INTEGER NOT NULL DEFAULT 0,
    bytes             INTEGER NOT NULL DEFAULT 0,
    wire_bytes        INTEGER NOT NULL DEFAULT 0,
    manifest_digest   TEXT    NOT NULL DEFAULT '',
    protected         TEXT    NOT NULL DEFAULT '',
    entries_omitted   INTEGER NOT NULL DEFAULT 0,
    receiver          TEXT    NOT NULL DEFAULT '',                   -- identity of the receiving leg
    receipt_hash      TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    deadline_at       INTEGER,                                       -- a non-terminal row becomes unknown after it
    ended_at          INTEGER,
    CHECK ((state IN ('admitted', 'prepared')) = (ended_at IS NULL))
);

CREATE INDEX file_transfers_created ON file_transfers (created_at, id);
CREATE INDEX file_transfers_principal ON file_transfers (principal_user_id, created_at);
CREATE INDEX file_transfers_source_owner ON file_transfers (source_owner_id, created_at);
CREATE INDEX file_transfers_target_owner ON file_transfers (target_owner_id, created_at);
CREATE INDEX file_transfers_open ON file_transfers (state, deadline_at) WHERE state IN ('admitted', 'prepared');

-- Up to 1,000 entries of a terminal transfer: relative path, size, SHA-256
-- (hex) or the reason it was skipped. They go with their transfer.
CREATE TABLE file_transfer_entries (
    transfer_id   TEXT    NOT NULL REFERENCES file_transfers(id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL CHECK (seq >= 0),
    path          TEXT    NOT NULL,
    size          INTEGER NOT NULL DEFAULT 0,
    sha256        TEXT    NOT NULL DEFAULT '',
    skipped_reason TEXT   NOT NULL DEFAULT '',
    PRIMARY KEY (transfer_id, seq)
);
