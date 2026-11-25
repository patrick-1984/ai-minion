-- Session grants (A104/A106, D1/D2/D4/D6): source session S on Worker A may
-- reach target T on Worker B at a level.
--
-- One row is one grant. source_incarnation '' means the source is identified
-- by its name (Claimed); another value is reserved for a bound-master
-- incarnation (Verified). target_session '' means every shared session of the
-- target Worker. level is a ladder: 1 View, 2 Message, 3 Control. authority
-- says what stands behind the grant: 'owner' (both Workers have the same
-- owner) or 'admin' (granted_by is an administrator). source_owner_id and
-- target_owner_id are the owners at decision time. Time columns are unix
-- milliseconds. With an empty table the Hub behaves as before: no existing
-- row is converted.
CREATE TABLE session_grant (
    id                 TEXT    PRIMARY KEY,                           -- UUIDv7
    source_worker_id   TEXT    NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    source_session     TEXT    NOT NULL CHECK (source_session <> ''),
    source_incarnation TEXT    NOT NULL DEFAULT '',
    target_worker_id   TEXT    NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    target_session     TEXT    NOT NULL DEFAULT '',
    level              INTEGER NOT NULL CHECK (level IN (1, 2, 3)),
    authority          TEXT    NOT NULL CHECK (authority IN ('owner', 'admin')),
    granted_by         TEXT    NOT NULL,
    source_owner_id    TEXT    NOT NULL,
    target_owner_id    TEXT    NOT NULL,
    note               TEXT    NOT NULL DEFAULT '',
    expires_at         INTEGER,
    revision           INTEGER NOT NULL DEFAULT 1,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    suspended_at       INTEGER,
    suspend_cause      TEXT    NOT NULL DEFAULT '',
    -- The local socket already joins the sessions of one Worker.
    CHECK (source_worker_id <> target_worker_id),
    CHECK (authority <> 'owner' OR source_owner_id = target_owner_id),
    UNIQUE (source_worker_id, source_session, source_incarnation, target_worker_id, target_session)
);

CREATE INDEX session_grant_source ON session_grant (source_worker_id, source_session);
CREATE INDEX session_grant_target ON session_grant (target_worker_id);
CREATE INDEX session_grant_granted_by ON session_grant (granted_by);

-- A Worker that changes owner loses every grant that touches it. A removed
-- Worker loses them by foreign key.
CREATE TRIGGER session_grant_worker_owner_changed AFTER UPDATE OF owner_id ON workers
WHEN NEW.owner_id <> OLD.owner_id
BEGIN
    DELETE FROM session_grant WHERE source_worker_id = NEW.id OR target_worker_id = NEW.id;
END;

-- When the authority behind an admin grant is lost, the grant is suspended in
-- the same transaction. No trigger lifts a suspension: a role or an account
-- that comes back does not bring the grant back, an administrator approves it
-- again. The first cause is kept.
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
