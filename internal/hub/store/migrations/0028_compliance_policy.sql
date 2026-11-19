-- v0.15.0: the effective compliance monitoring policy (A49 T15). One row. The
-- version increases by one with every effective change (level, input,
-- destination, traffic payload), in the same transaction as the settings rows
-- and the audit rows of the change, so the audit trail can say from which
-- version on which content was recorded. The row holds no credential and no
-- content: dest_label is scheme://host:port.
CREATE TABLE compliance_policy (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    version         INTEGER NOT NULL CHECK (version >= 0),
    level           TEXT    NOT NULL CHECK (level IN ('off', 'actions', 'output')),
    input           INTEGER NOT NULL CHECK (input IN (0, 1)),
    traffic_payload INTEGER NOT NULL CHECK (traffic_payload IN (0, 1)),
    dest_label      TEXT    NOT NULL,
    updated_at      INTEGER NOT NULL,           -- unix milliseconds
    updated_by      TEXT    NOT NULL
);
