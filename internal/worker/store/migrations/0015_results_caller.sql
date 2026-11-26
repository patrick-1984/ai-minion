-- Who sent a cached write and what it carried (audit F13, story W5).
--
-- op_results was keyed by request_id and compared only op, session and force,
-- so a second caller that reused a request_id, or the same caller with other
-- input, got the first request's outcome and its own input was never sent.
-- caller names the sender ("local", or the identity members of the Hub-derived
-- origin: kind, user, acting admin, API key, source Worker); payload_hash is a
-- SHA-256 digest of the canonical arguments, never the arguments or the text.
-- A repeated request_id with another caller or another digest is a conflict.
-- Rows written before this migration keep '' in both and compare as before;
-- they expire within ten minutes.
ALTER TABLE op_results ADD COLUMN caller TEXT NOT NULL DEFAULT '';
ALTER TABLE op_results ADD COLUMN payload_hash TEXT NOT NULL DEFAULT '';
