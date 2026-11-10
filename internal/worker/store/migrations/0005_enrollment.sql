-- Hub enrollment of the Worker (Story 4.4, AD-5, AD-9): the pinned Hub key and
-- its position in the transition chain, and what the Hub said in welcome
-- (worker id, name, owner, kind and the Hub policy). At most one row: a Worker
-- belongs to one Hub. The row is written when the Worker is enrolled (invitation
-- or pairing code) and deleted only by a signed revoke or by "minion worker
-- reset" (AD-9); nothing time-based touches it. The Worker's own identity key
-- is a private file, not a column.
CREATE TABLE hub_enrollment (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    hub_pub      BLOB    NOT NULL CHECK (length(hub_pub) = 32),
    pin_seq      INTEGER NOT NULL CHECK (pin_seq >= 0),
    worker_id    TEXT    NOT NULL DEFAULT '',
    worker_name  TEXT    NOT NULL DEFAULT '',
    owner        TEXT    NOT NULL DEFAULT '',
    kind         TEXT    NOT NULL DEFAULT '',
    preview_only INTEGER NOT NULL DEFAULT 0 CHECK (preview_only IN (0, 1)),
    enrolled_at  INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);
