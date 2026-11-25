-- Grants that end or are suspended without a person's operation leave an
-- audit row (actor 'system'), and a grant remembers when it was last used.
--
-- The rows written here have the shape the grant handlers write: grant_id,
-- source "<source worker id>:<source session>", target "<target worker
-- id>:<session or *>", detail "cause=<cause>". The session purge and the
-- expiry sweep write theirs in Go (session_grant_audit.go).
ALTER TABLE session_grant ADD COLUMN last_used_at INTEGER;
ALTER TABLE session_grant ADD COLUMN use_count INTEGER NOT NULL DEFAULT 0;

-- The current time in unix milliseconds, for the triggers below.
CREATE VIEW grant_audit_now AS
SELECT CAST(strftime('%s', 'now') AS INTEGER) * 1000 + CAST(substr(strftime('%f', 'now'), 4) AS INTEGER) AS ms;

-- A Worker that changes owner loses every grant that touches it (0032); the
-- trigger now records each of them first.
DROP TRIGGER session_grant_worker_owner_changed;
CREATE TRIGGER session_grant_worker_owner_changed AFTER UPDATE OF owner_id ON workers
WHEN NEW.owner_id <> OLD.owner_id
BEGIN
    INSERT INTO audit_events (id, ts, actor, action, target, outcome, source, grant_id, detail)
    SELECT lower(printf('%s-%s-7%s-%s%s-%s', substr(printf('%012x', n.ms), 1, 8), substr(printf('%012x', n.ms), 9, 4),
               substr(hex(randomblob(2)), 1, 3), substr('89ab', 1 + (random() & 3), 1),
               substr(hex(randomblob(2)), 1, 3), hex(randomblob(6)))),
           n.ms, 'system', 'grant.end',
           g.target_worker_id || ':' || CASE WHEN g.target_session = '' THEN '*' ELSE g.target_session END,
           'success', g.source_worker_id || ':' || g.source_session, g.id, 'cause=owner_changed'
    FROM session_grant g, grant_audit_now n
    WHERE g.source_worker_id = NEW.id OR g.target_worker_id = NEW.id;
    DELETE FROM session_grant WHERE source_worker_id = NEW.id OR target_worker_id = NEW.id;
END;

-- A removed Worker loses its grants by foreign key; they are recorded before
-- the cascade deletes them.
CREATE TRIGGER session_grant_worker_removed BEFORE DELETE ON workers
BEGIN
    INSERT INTO audit_events (id, ts, actor, action, target, outcome, source, grant_id, detail)
    SELECT lower(printf('%s-%s-7%s-%s%s-%s', substr(printf('%012x', n.ms), 1, 8), substr(printf('%012x', n.ms), 9, 4),
               substr(hex(randomblob(2)), 1, 3), substr('89ab', 1 + (random() & 3), 1),
               substr(hex(randomblob(2)), 1, 3), hex(randomblob(6)))),
           n.ms, 'system', 'grant.end',
           g.target_worker_id || ':' || CASE WHEN g.target_session = '' THEN '*' ELSE g.target_session END,
           'success', g.source_worker_id || ':' || g.source_session, g.id, 'cause=worker_removed'
    FROM session_grant g, grant_audit_now n
    WHERE g.source_worker_id = OLD.id OR g.target_worker_id = OLD.id;
END;

-- A suspension by one of the triggers of 0032 (the authority behind an admin
-- grant is lost). A suspension with another cause is not a system cause.
CREATE TRIGGER session_grant_suspended_audit AFTER UPDATE OF suspended_at ON session_grant
WHEN OLD.suspended_at IS NULL AND NEW.suspended_at IS NOT NULL
     AND NEW.suspend_cause IN ('grantor_not_admin', 'grantor_inactive')
BEGIN
    INSERT INTO audit_events (id, ts, actor, action, target, outcome, source, grant_id, detail)
    SELECT lower(printf('%s-%s-7%s-%s%s-%s', substr(printf('%012x', n.ms), 1, 8), substr(printf('%012x', n.ms), 9, 4),
               substr(hex(randomblob(2)), 1, 3), substr('89ab', 1 + (random() & 3), 1),
               substr(hex(randomblob(2)), 1, 3), hex(randomblob(6)))),
           n.ms, 'system', 'grant.suspend',
           NEW.target_worker_id || ':' || CASE WHEN NEW.target_session = '' THEN '*' ELSE NEW.target_session END,
           'success', NEW.source_worker_id || ':' || NEW.source_session, NEW.id, 'cause=' || NEW.suspend_cause
    FROM grant_audit_now n;
END;
