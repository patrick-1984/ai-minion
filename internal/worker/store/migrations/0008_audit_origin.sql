-- The origin of a Hub request (protocol 2.0, F-01): the JSON the Hub derived
-- from the authenticated caller, or "local" for a local caller, or "missing" /
-- "malformed" for a refused request whose origin was not usable. It never holds
-- terminal content, message text, keys or secrets (AD-20).
ALTER TABLE local_audit ADD COLUMN origin TEXT NOT NULL DEFAULT '';
