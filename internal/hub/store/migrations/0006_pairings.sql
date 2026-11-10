-- Story 4.8: pairing codes on the Hub (AD-8, AD-16). The workers-owned table
-- pending_pairings holds one row per public pairing id (the first four
-- characters of a pairing code) that a Worker has announced. The row carries what must survive a Hub restart: the expiry of
-- the code, the number of failed attempts, and whether the code was used or
-- dropped. It never holds the code, the CPace password or any key; the
-- pending connection itself lives only in memory. Only the workers domain
-- package writes this table.
CREATE TABLE pending_pairings (
    pairing_id TEXT    PRIMARY KEY,          -- public id, four Crockford characters
    status     TEXT    NOT NULL CHECK (status IN ('pending', 'used', 'dropped')),
    attempts   INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    created_at INTEGER NOT NULL,             -- unix milliseconds
    expires_at INTEGER NOT NULL,             -- unix milliseconds; a code lives 10 minutes
    owner_id   TEXT    NOT NULL DEFAULT '',  -- set when used: the user that claimed it
    worker_id  TEXT    NOT NULL DEFAULT ''   -- set when used: the Worker it created
);

CREATE INDEX pending_pairings_expires ON pending_pairings (expires_at);

-- A code that created a Worker stays dead after its row expired and was
-- purged: pending_pairings rows are reusable public ids, so the used flag of
-- the row alone would let the same code work again. Only a one-way digest of
-- the code is kept (never the code or its CPace password), and it is kept for
-- good: a used code can be neither guessed nor replayed.
CREATE TABLE used_pairing_codes (
    code_digest TEXT    PRIMARY KEY,         -- hex SHA-256 of the canonical code, domain separated
    used_at     INTEGER NOT NULL             -- unix milliseconds
);
