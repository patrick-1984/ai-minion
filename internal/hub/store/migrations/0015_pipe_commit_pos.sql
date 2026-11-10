-- Story 7.4 integration (review round 2): the Hub pipe orders its entries by
-- one shared commit position, and a pipe_send keeps its outcome.
--
-- 1. Position. The messages and the turns of a peer were ordered by Hub time,
--    then message before turn, then insertion order. A message inserted in
--    the same millisecond as a turn that a reader already holds as its cursor
--    sorted before that turn and was skipped for ever. Every entry now gets a
--    position from one counter, assigned in the same transaction as its
--    insert (writers are serialised, so the position grows with the commit),
--    and a page is read after the position of its cursor. A reader never sees
--    position N+1 without N.
-- 2. Result. A pipe_send keeps the WriteResult the Worker answered with, next
--    to its request_id. A retry (the Hub keeps the row for 7 days, the Worker
--    its request_id cache for 10 minutes) is answered from the row and is not
--    routed to the Worker again.

CREATE TABLE pipe_commit (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    n  INTEGER NOT NULL
);

ALTER TABLE pipe_messages ADD COLUMN pos INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pipe_turns    ADD COLUMN pos INTEGER NOT NULL DEFAULT 0;
-- The JSON WriteResult of the Worker's answer; NULL while the send is pending
-- (or when it ended without an answer, which the Hub treats as unknown).
ALTER TABLE pipe_messages ADD COLUMN send_result TEXT;

-- Rows of an earlier schema keep the order they had: Hub time, message before
-- turn, then id.
CREATE TEMP TABLE pipe_pos_seed AS
    SELECT rank, rid, ROW_NUMBER() OVER (ORDER BY at, rank, rid) AS n
      FROM (SELECT 0 AS rank, id  AS rid, at FROM pipe_messages
            UNION ALL
            SELECT 1 AS rank, seq AS rid, at FROM pipe_turns);
CREATE INDEX pipe_pos_seed_key ON pipe_pos_seed (rank, rid);
UPDATE pipe_messages SET pos = (SELECT n FROM pipe_pos_seed WHERE rank = 0 AND rid = pipe_messages.id);
UPDATE pipe_turns    SET pos = (SELECT n FROM pipe_pos_seed WHERE rank = 1 AND rid = pipe_turns.seq);
INSERT INTO pipe_commit (id, n) VALUES (1, (SELECT COALESCE(MAX(n), 0) FROM pipe_pos_seed));
DROP TABLE pipe_pos_seed;

CREATE UNIQUE INDEX pipe_messages_pos      ON pipe_messages (pos);
CREATE UNIQUE INDEX pipe_turns_pos         ON pipe_turns (pos);
CREATE INDEX        pipe_messages_peer_pos ON pipe_messages (worker_id, session_name, pos);
CREATE INDEX        pipe_turns_peer_pos    ON pipe_turns (worker_id, session_name, pos);

-- Whatever inserts a row (the broker, a test) gets the next position: the
-- trigger runs in the transaction of the insert.
CREATE TRIGGER pipe_messages_pos_assign AFTER INSERT ON pipe_messages WHEN NEW.pos = 0
BEGIN
    UPDATE pipe_commit SET n = n + 1 WHERE id = 1;
    UPDATE pipe_messages SET pos = (SELECT n FROM pipe_commit WHERE id = 1) WHERE id = NEW.id;
END;

CREATE TRIGGER pipe_turns_pos_assign AFTER INSERT ON pipe_turns WHEN NEW.pos = 0
BEGIN
    UPDATE pipe_commit SET n = n + 1 WHERE id = 1;
    UPDATE pipe_turns SET pos = (SELECT n FROM pipe_commit WHERE id = 1) WHERE seq = NEW.seq;
END;
