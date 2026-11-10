-- A turn answers a delivery: the Hub names the message it delivered
-- (message_id of the routed pipe_send) and accepts a turn only with it. The
-- outbox keeps the id with the turn. A turn that was queued before this
-- migration has none (0): the Hub rejects it for good and the outbox drops it.
ALTER TABLE pipe_outbox ADD COLUMN message_id INTEGER NOT NULL DEFAULT 0;
