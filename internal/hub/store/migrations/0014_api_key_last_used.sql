-- Story 6.7 (integration): the last use of an API key, shown in the key list of
-- the web UI. NULL until the key has authenticated once. Unix milliseconds. The
-- apikey domain owns the column; it is written by Store.TouchAPIKey, at most
-- once per minute for a key, so a busy key does not turn every request into a
-- database write.
ALTER TABLE api_keys ADD COLUMN last_used_at INTEGER;
