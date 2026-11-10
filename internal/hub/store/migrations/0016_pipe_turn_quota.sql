-- Security: a Worker may store only replies to deliveries, within quotas.
--
-- 1. A turn answers one delivery: message_id is the pipe_messages row that the
--    Hub sent to the session and the Worker is replying to. A message is
--    answered at most once (the unique index), and the Hub accepts a turn only
--    for a message of the Worker that sends it (internal/hub/workers/pipe.go).
--    NULL for the turns stored before this migration.
-- 2. Storage quotas. cost is what a turn occupies (its text and a fixed row
--    overhead); owner_id is the owner of the Worker when the turn was stored.
--    pipe_turn_usage holds the sum per scope ('worker', 'owner', 'global'),
--    kept by the triggers below in the transaction of every insert and delete
--    (so retention pruning gives the quota back), and read by the quota check
--    in the transaction of the insert. Turns stored before this migration have
--    no owner and count for their Worker and for the Hub.

ALTER TABLE pipe_turns ADD COLUMN message_id INTEGER;
ALTER TABLE pipe_turns ADD COLUMN owner_id   TEXT    NOT NULL DEFAULT '';
ALTER TABLE pipe_turns ADD COLUMN cost       INTEGER NOT NULL DEFAULT 0;

UPDATE pipe_turns SET cost = length(CAST(text AS BLOB)) + 256;

CREATE UNIQUE INDEX pipe_turns_message ON pipe_turns (message_id) WHERE message_id IS NOT NULL;

CREATE TABLE pipe_turn_usage (
    scope TEXT    NOT NULL CHECK (scope IN ('worker', 'owner', 'global')),
    key   TEXT    NOT NULL,
    turns INTEGER NOT NULL CHECK (turns >= 0),
    cost  INTEGER NOT NULL CHECK (cost >= 0),
    PRIMARY KEY (scope, key)
) WITHOUT ROWID;

INSERT INTO pipe_turn_usage (scope, key, turns, cost)
    SELECT 'worker', worker_id, COUNT(*), SUM(cost) FROM pipe_turns GROUP BY worker_id;
INSERT INTO pipe_turn_usage (scope, key, turns, cost)
    SELECT 'global', '', COUNT(*), SUM(cost) FROM pipe_turns HAVING COUNT(*) > 0;

CREATE TRIGGER pipe_turns_usage_insert AFTER INSERT ON pipe_turns
BEGIN
    INSERT INTO pipe_turn_usage (scope, key, turns, cost) VALUES ('worker', NEW.worker_id, 1, NEW.cost)
        ON CONFLICT (scope, key) DO UPDATE SET turns = turns + 1, cost = cost + NEW.cost;
    INSERT INTO pipe_turn_usage (scope, key, turns, cost) VALUES ('global', '', 1, NEW.cost)
        ON CONFLICT (scope, key) DO UPDATE SET turns = turns + 1, cost = cost + NEW.cost;
    INSERT INTO pipe_turn_usage (scope, key, turns, cost) SELECT 'owner', NEW.owner_id, 1, NEW.cost WHERE NEW.owner_id <> ''
        ON CONFLICT (scope, key) DO UPDATE SET turns = turns + 1, cost = cost + NEW.cost;
END;

CREATE TRIGGER pipe_turns_usage_delete AFTER DELETE ON pipe_turns
BEGIN
    UPDATE pipe_turn_usage SET turns = MAX(turns - 1, 0), cost = MAX(cost - OLD.cost, 0)
        WHERE (scope = 'worker' AND key = OLD.worker_id)
           OR (scope = 'global' AND key = '')
           OR (scope = 'owner' AND key = OLD.owner_id AND OLD.owner_id <> '');
END;
