-- S4.15 residual hardening: a restrictive share_set that the Hub dispatched and
-- whose outcome is not yet settled, kept durably. The row is written BEFORE the
-- request is sent to the Worker and removed only when the outcome is settled by
-- evidence that is ordered after the request (the Worker's own answer to it, a
-- newer change, or the Worker's answer to a probe that raised its barrier past
-- seq). A Hub restart reads these rows back and keeps what each rule would hide
-- hidden, so that a restriction whose outcome is unknown is never lifted by a
-- restart. seq is the Hub's sequence number of the change (it also travels to
-- the Worker as op_seq and orders the changes of the Worker's barrier); it grows
-- across restarts. mode and names are the rule that was asked for.
CREATE TABLE worker_share_pending (
    seq          INTEGER NOT NULL PRIMARY KEY,
    worker_id    TEXT    NOT NULL,
    mode         TEXT    NOT NULL CHECK (mode IN ('all', 'none', 'select', 'later')),
    names        TEXT    NOT NULL DEFAULT '[]',
    base_revision INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL
);
CREATE INDEX worker_share_pending_worker ON worker_share_pending (worker_id);
