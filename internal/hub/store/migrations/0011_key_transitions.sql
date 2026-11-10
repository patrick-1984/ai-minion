-- Story 4.9: the Hub key transition chain (AD-5). Each planned rotation of the
-- Hub identity appends one link, signed by the key it replaces (purpose
-- "transition"). A connecting Worker that is pinned to any older key receives
-- the chain and follows it to the current key. Only the workers domain package
-- writes this table, in the same transaction that updates hub_identity.

CREATE TABLE hub_key_transitions (
    seq        INTEGER PRIMARY KEY CHECK (seq >= 1),          -- 1 for the first rotation
    old_pub    BLOB    NOT NULL UNIQUE CHECK (length(old_pub) = 32),
    new_pub    BLOB    NOT NULL UNIQUE CHECK (length(new_pub) = 32),
    created_at INTEGER NOT NULL,                              -- unix milliseconds
    sig_by_old BLOB    NOT NULL CHECK (length(sig_by_old) = 64),
    CHECK (old_pub <> new_pub)
);
