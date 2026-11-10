-- The Worker's pipe (Story 7.3, AD-23, AD-16). The number 0004 was reserved for
-- this story; the runner applies a migration that a database lacks whatever
-- the order of the numbers, so a database that already holds 0005 takes it.
--
-- pipe_entries is the Solo broker's store: the messages sent to a session
-- (kind 'sent') and the replies the turn extractor read (kind 'received') in
-- one commit order (pos). It holds the text of the pipe, which is the point of
-- the pipe, and never anything else of the terminal. Every entry has an
-- entry_id (a UUIDv7) so that every entry is a valid cursor of pipe_read and
-- pipe_wait. Hub mode does not use it: the Hub is the broker there.
CREATE TABLE pipe_entries (
    pos          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT    NOT NULL CHECK (kind IN ('sent', 'received')),
    entry_id     TEXT    NOT NULL UNIQUE CHECK (length(entry_id) = 36),
    worker_id    TEXT    NOT NULL CHECK (length(worker_id) > 0),
    session_name TEXT    NOT NULL CHECK (length(session_name) > 0),
    text         TEXT    NOT NULL,
    at_ms        INTEGER NOT NULL
);

CREATE INDEX pipe_entries_peer ON pipe_entries (worker_id, session_name, pos);
CREATE INDEX pipe_entries_at ON pipe_entries (at_ms);

-- pipe_outbox is the durable outbox of the turns of a shared session that the
-- Hub has not acknowledged (turn.ack): a turn leaves it only with the ack, so
-- a restart of the Worker never loses a reply. It belongs to the enrollment:
-- "minion worker reset" empties it.
CREATE TABLE pipe_outbox (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    turn_id      TEXT    NOT NULL UNIQUE CHECK (length(turn_id) = 36),
    session_name TEXT    NOT NULL CHECK (length(session_name) > 0),
    text         TEXT    NOT NULL,
    at_ms        INTEGER NOT NULL
);
