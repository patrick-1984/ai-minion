-- v0.23.0: an invitation key can name the Worker it enrolls. The Hub stores
-- that name for the new Worker whatever name the enrolling Worker announces
-- (a Worker that was given only the key, like the Android app, announces its
-- own name). Empty means the Worker's own name, as before.
ALTER TABLE invitation_keys ADD COLUMN worker_name TEXT NOT NULL DEFAULT '';

-- The one-step join command of the Add worker page used its one name field
-- for the key and for the Worker, and so does a one-time key: an unused
-- one-time user key keeps its name as the Worker name. Enrollment uses it only
-- when it is a valid Worker name (no ':', no whitespace), so a free-text
-- label such as "lab worker setup" still leaves the Worker its own name.
UPDATE invitation_keys
   SET worker_name = name
 WHERE mode = 'one_time' AND source = 'user' AND uses = 0 AND revoked = 0 AND name <> ''
   AND instr(name, ':') = 0 AND instr(name, ' ') = 0
   AND length(CAST(name AS BLOB)) <= 64;
