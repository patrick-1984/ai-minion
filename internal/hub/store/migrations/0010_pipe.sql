-- Story 7.4: the Hub pipe (AD-23, R26-R29). In Hub mode the Hub is the broker
-- (internal/pipe) and these two tables are its store. All times are unix
-- milliseconds. The peer of a row is {worker_id, session_name}; worker_id has
-- no foreign key: the workers table belongs to another domain, and messages
-- and turns are deleted by retention (7 days) and not with the Worker.

-- Messages sent to a session. id grows strictly with insertion order. The
-- task budget is the count of these rows per peer in the last 24 hours.
CREATE TABLE pipe_messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    worker_id    TEXT    NOT NULL,
    session_name TEXT    NOT NULL,
    text         TEXT    NOT NULL,
    at           INTEGER NOT NULL,
    -- The request_id of the pipe_send that stored the row. A retry with the
    -- same id (after a lost answer) finds the row and is neither stored nor
    -- counted again. NULL for rows that were stored without one.
    request_id   TEXT,
    CHECK (worker_id <> '' AND session_name <> '')
);

CREATE INDEX pipe_messages_peer ON pipe_messages (worker_id, session_name, id);
CREATE INDEX pipe_messages_at   ON pipe_messages (at);
CREATE UNIQUE INDEX pipe_messages_request ON pipe_messages (worker_id, session_name, request_id)
    WHERE request_id IS NOT NULL;

-- Turns extracted on a Worker and committed idempotently by turn_id (a
-- UUIDv7). at is the time the Hub committed the turn (the Hub clock, never
-- the Worker's), so that messages and turns sort on one clock. seq is the
-- commit order that breaks ties.
CREATE TABLE pipe_turns (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    turn_id      TEXT    NOT NULL UNIQUE,
    worker_id    TEXT    NOT NULL,
    session_name TEXT    NOT NULL,
    text         TEXT    NOT NULL,
    at           INTEGER NOT NULL,
    CHECK (turn_id <> '' AND worker_id <> '' AND session_name <> '')
);

CREATE INDEX pipe_turns_peer ON pipe_turns (worker_id, session_name, seq);
CREATE INDEX pipe_turns_at   ON pipe_turns (at);
