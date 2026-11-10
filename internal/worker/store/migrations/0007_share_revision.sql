-- Revision of the share selection (S4.15 privacy hardening). It rises by one
-- with every committed change of the share choice (SaveShare, and a pairing
-- reset that removes the choice), in the same transaction, and is never reset:
-- it lives outside share_config, which a pairing reset deletes. The Hub orders
-- the share rules a Worker reports by it, so that a late or replayed answer
-- cannot undo a newer choice.
CREATE TABLE share_revision (
    id       INTEGER PRIMARY KEY CHECK (id = 1),
    revision INTEGER NOT NULL CHECK (revision >= 1)
);

-- Revisions start at 1: 0 is what a Worker without revisions reports.
INSERT INTO share_revision (id, revision) VALUES (1, 1);
