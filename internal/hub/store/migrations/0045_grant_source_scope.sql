-- Grant source selectors and one-step grants (v0.26.0).
--
-- 1. session_grant is rebuilt with an explicit source_scope:
--      session   one source session: source_worker_id and source_session set;
--      worker    every shared session of one source Worker: source_worker_id
--                set, no session name;
--      everyone  every shared session of every eligible Worker: no source
--                Worker, session or source owner pin; admin authority only.
--    A missing source never means everyone: the scope is explicit. The target
--    stays a real Worker with one named session, or '' for every shared
--    session of that Worker. Every pair is cross-Worker.
--
--    The file and app flags no longer need a bound (verified) source: a new
--    grant carries them from its creation. files_push and apps_install still
--    need Control, command_shell still needs Control on one named session.
--    A nonempty source_incarnation of an existing row is kept as a legacy
--    restriction of that row: it is used only by that terminal incarnation.
--
--    Every row is copied with its id, flags, revision, times and usage, as
--    source_scope 'session'. The triggers of 0032 and 0034 are recreated for
--    the new table.
DROP TRIGGER session_grant_worker_owner_changed;
DROP TRIGGER session_grant_worker_removed;
DROP TRIGGER session_grant_admin_role_deleted;
DROP TRIGGER session_grant_admin_role_updated;
DROP TRIGGER session_grant_grantor_deactivated;
DROP TRIGGER session_grant_suspended_audit;

CREATE TABLE session_grant_v2 (
    id                 TEXT    PRIMARY KEY,
    source_scope       TEXT    NOT NULL CHECK (source_scope IN ('session', 'worker', 'everyone')),
    source_worker_id   TEXT    REFERENCES workers(id) ON DELETE CASCADE,
    source_session     TEXT    NOT NULL DEFAULT '',
    source_incarnation TEXT    NOT NULL DEFAULT '',
    target_worker_id   TEXT    NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    target_session     TEXT    NOT NULL DEFAULT '',
    level              INTEGER NOT NULL CHECK (level IN (1, 2, 3)),
    authority          TEXT    NOT NULL CHECK (authority IN ('owner', 'admin')),
    granted_by         TEXT    NOT NULL,
    source_owner_id    TEXT    NOT NULL DEFAULT '',
    target_owner_id    TEXT    NOT NULL,
    note               TEXT    NOT NULL DEFAULT '',
    expires_at         INTEGER,
    revision           INTEGER NOT NULL DEFAULT 1,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    suspended_at       INTEGER,
    suspend_cause      TEXT    NOT NULL DEFAULT '',
    last_used_at       INTEGER,
    use_count          INTEGER NOT NULL DEFAULT 0,
    command_shell      INTEGER NOT NULL DEFAULT 0
        CHECK (command_shell = 0 OR (command_shell = 1 AND level = 3 AND target_session <> '')),
    files_pull         INTEGER NOT NULL DEFAULT 0 CHECK (files_pull IN (0, 1)),
    files_push         INTEGER NOT NULL DEFAULT 0 CHECK (files_push = 0 OR (files_push = 1 AND level = 3)),
    apps_install       INTEGER NOT NULL DEFAULT 0 CHECK (apps_install = 0 OR (apps_install = 1 AND level = 3)),
    -- The shape of each source selector.
    CHECK (
        (source_scope = 'session' AND source_worker_id IS NOT NULL AND source_session <> '' AND source_owner_id <> '')
        OR (source_scope = 'worker' AND source_worker_id IS NOT NULL AND source_session = '' AND source_incarnation = ''
            AND source_owner_id <> '')
        OR (source_scope = 'everyone' AND source_worker_id IS NULL AND source_session = '' AND source_incarnation = ''
            AND source_owner_id = '' AND authority = 'admin')
    ),
    -- The local socket already joins the sessions of one Worker.
    CHECK (source_worker_id IS NULL OR source_worker_id <> target_worker_id),
    CHECK (authority <> 'owner' OR source_owner_id = target_owner_id)
);

INSERT INTO session_grant_v2 (id, source_scope, source_worker_id, source_session, source_incarnation, target_worker_id,
    target_session, level, authority, granted_by, source_owner_id, target_owner_id, note, expires_at, revision,
    created_at, updated_at, suspended_at, suspend_cause, last_used_at, use_count, command_shell, files_pull, files_push,
    apps_install)
SELECT id, 'session', source_worker_id, source_session, source_incarnation, target_worker_id,
    target_session, level, authority, granted_by, source_owner_id, target_owner_id, note, expires_at, revision,
    created_at, updated_at, suspended_at, suspend_cause, last_used_at, use_count, command_shell, files_pull, files_push,
    apps_install
FROM session_grant;

DROP TABLE session_grant;
ALTER TABLE session_grant_v2 RENAME TO session_grant;

-- One grant per selector pair. Distinct legacy incarnations stay distinct.
CREATE UNIQUE INDEX session_grant_unique_session ON session_grant
    (source_worker_id, source_session, source_incarnation, target_worker_id, target_session) WHERE source_scope = 'session';
CREATE UNIQUE INDEX session_grant_unique_worker ON session_grant
    (source_worker_id, target_worker_id, target_session) WHERE source_scope = 'worker';
CREATE UNIQUE INDEX session_grant_unique_everyone ON session_grant
    (target_worker_id, target_session) WHERE source_scope = 'everyone';
CREATE INDEX session_grant_source ON session_grant (source_worker_id, source_session);
CREATE INDEX session_grant_target ON session_grant (target_worker_id);
CREATE INDEX session_grant_granted_by ON session_grant (granted_by);

-- The audit source of a grant: "<worker>:<session>", "<worker>:*" for every
-- shared session of the source Worker, "*" for everyone.
CREATE TRIGGER session_grant_worker_owner_changed AFTER UPDATE OF owner_id ON workers
WHEN NEW.owner_id <> OLD.owner_id
BEGIN
    INSERT INTO audit_events (id, ts, actor, action, target, outcome, source, grant_id, detail)
    SELECT lower(printf('%s-%s-7%s-%s%s-%s', substr(printf('%012x', n.ms), 1, 8), substr(printf('%012x', n.ms), 9, 4),
               substr(hex(randomblob(2)), 1, 3), substr('89ab', 1 + (random() & 3), 1),
               substr(hex(randomblob(2)), 1, 3), hex(randomblob(6)))),
           n.ms, 'system', 'grant.end',
           g.target_worker_id || ':' || CASE WHEN g.target_session = '' THEN '*' ELSE g.target_session END,
           'success',
           CASE g.source_scope WHEN 'everyone' THEN '*' WHEN 'worker' THEN g.source_worker_id || ':*'
                ELSE g.source_worker_id || ':' || g.source_session END,
           g.id, 'cause=owner_changed'
    FROM session_grant g, grant_audit_now n
    WHERE g.source_worker_id = NEW.id OR g.target_worker_id = NEW.id;
    DELETE FROM session_grant WHERE source_worker_id = NEW.id OR target_worker_id = NEW.id;
END;

CREATE TRIGGER session_grant_worker_removed BEFORE DELETE ON workers
BEGIN
    INSERT INTO audit_events (id, ts, actor, action, target, outcome, source, grant_id, detail)
    SELECT lower(printf('%s-%s-7%s-%s%s-%s', substr(printf('%012x', n.ms), 1, 8), substr(printf('%012x', n.ms), 9, 4),
               substr(hex(randomblob(2)), 1, 3), substr('89ab', 1 + (random() & 3), 1),
               substr(hex(randomblob(2)), 1, 3), hex(randomblob(6)))),
           n.ms, 'system', 'grant.end',
           g.target_worker_id || ':' || CASE WHEN g.target_session = '' THEN '*' ELSE g.target_session END,
           'success',
           CASE g.source_scope WHEN 'everyone' THEN '*' WHEN 'worker' THEN g.source_worker_id || ':*'
                ELSE g.source_worker_id || ':' || g.source_session END,
           g.id, 'cause=worker_removed'
    FROM session_grant g, grant_audit_now n
    WHERE g.source_worker_id = OLD.id OR g.target_worker_id = OLD.id;
END;

CREATE TRIGGER session_grant_admin_role_deleted AFTER DELETE ON user_roles
WHEN OLD.role = 'admin'
     AND NOT EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = OLD.user_id AND r.role = 'admin')
BEGIN
    UPDATE session_grant
    SET suspended_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000,
        suspend_cause = 'grantor_not_admin', revision = revision + 1
    WHERE authority = 'admin' AND granted_by = OLD.user_id AND suspended_at IS NULL;
END;

CREATE TRIGGER session_grant_admin_role_updated AFTER UPDATE ON user_roles
WHEN OLD.role = 'admin'
     AND NOT EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = OLD.user_id AND r.role = 'admin')
BEGIN
    UPDATE session_grant
    SET suspended_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000,
        suspend_cause = 'grantor_not_admin', revision = revision + 1
    WHERE authority = 'admin' AND granted_by = OLD.user_id AND suspended_at IS NULL;
END;

CREATE TRIGGER session_grant_grantor_deactivated AFTER UPDATE OF active ON users
WHEN OLD.active = 1 AND NEW.active = 0
BEGIN
    UPDATE session_grant
    SET suspended_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000,
        suspend_cause = 'grantor_inactive', revision = revision + 1
    WHERE authority = 'admin' AND granted_by = NEW.id AND suspended_at IS NULL;
END;

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
           'success',
           CASE NEW.source_scope WHEN 'everyone' THEN '*' WHEN 'worker' THEN NEW.source_worker_id || ':*'
                ELSE NEW.source_worker_id || ':' || NEW.source_session END,
           NEW.id, 'cause=' || NEW.suspend_cause
    FROM grant_audit_now n;
END;

-- 2. Master acceptance flags. Until now a flag of an acceptance counted only
--    for a bound master lease. A flag that is on keeps that restriction as a
--    migration-only marker (*_bound = 1): it still counts only for a bound
--    master lease. A flag switched on from now on (from off) counts for the
--    nominated master sessions of the source Worker, whatever their lane;
--    switching a flag off clears its marker. Editing the note, the session
--    list or another flag leaves a marker as it is.
ALTER TABLE control_accept ADD COLUMN files_pull_bound INTEGER NOT NULL DEFAULT 0 CHECK (files_pull_bound IN (0, 1));
ALTER TABLE control_accept ADD COLUMN files_push_bound INTEGER NOT NULL DEFAULT 0 CHECK (files_push_bound IN (0, 1));
ALTER TABLE control_accept ADD COLUMN apps_install_bound INTEGER NOT NULL DEFAULT 0 CHECK (apps_install_bound IN (0, 1));
UPDATE control_accept SET files_pull_bound = files_pull, files_push_bound = files_push, apps_install_bound = apps_install;
