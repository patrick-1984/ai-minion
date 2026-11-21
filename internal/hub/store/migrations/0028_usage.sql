-- Per-Worker attributes and latest successful full session observation.
CREATE TABLE usage_workers (
    worker_id TEXT PRIMARY KEY REFERENCES workers(id) ON DELETE CASCADE,
    observed_at INTEGER NOT NULL,
    complete_at INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'ready',
    inventory_at INTEGER,
    inventory TEXT
);
-- id identifies a session incarnation across the domain. Only a registered
-- Worker can supply its local_id, the Hub constructs the domain id.
CREATE TABLE usage_sessions (
    id TEXT PRIMARY KEY,
    worker_id TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    observed_at INTEGER NOT NULL,
    ended_at INTEGER,
    body TEXT NOT NULL
);
CREATE INDEX usage_sessions_worker ON usage_sessions(worker_id, id);
CREATE INDEX usage_sessions_ended ON usage_sessions(ended_at);
-- Latest cumulative counters per session in each observed UTC hour. Read
-- adjacent samples to calculate deltas; never sum cumulative snapshots.
CREATE TABLE usage_hours (
    session_id TEXT NOT NULL REFERENCES usage_sessions(id) ON DELETE CASCADE,
    hour INTEGER NOT NULL,
    account_id TEXT NOT NULL DEFAULT '',
    observed_at INTEGER NOT NULL,
    counters TEXT NOT NULL,
    PRIMARY KEY (session_id, hour)
) WITHOUT ROWID;
CREATE INDEX usage_hours_age ON usage_hours(hour);
CREATE TABLE usage_accounts (
    worker_id TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL,
    can_poll INTEGER NOT NULL DEFAULT 0,
    observed_at INTEGER NOT NULL,
    body TEXT NOT NULL,
    PRIMARY KEY (worker_id, account_id)
) WITHOUT ROWID;
CREATE INDEX usage_accounts_identity ON usage_accounts(account_id, observed_at);
CREATE TABLE usage_buckets (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK(kind IN ('session','provider')),
    label TEXT NOT NULL,
    position INTEGER NOT NULL DEFAULT 0,
    UNIQUE(owner_id, kind, id),
    UNIQUE(owner_id, kind, label)
);
CREATE TABLE usage_members (
    owner_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    member_id TEXT NOT NULL,
    bucket_id TEXT NOT NULL,
    PRIMARY KEY(owner_id, kind, member_id),
    FOREIGN KEY(owner_id,kind,bucket_id) REFERENCES usage_buckets(owner_id,kind,id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX usage_members_bucket ON usage_members(bucket_id);
-- Reassignment is also a data boundary. Historical metadata and private
-- memberships do not follow an infrastructure Worker to its next owner.
CREATE TRIGGER usage_worker_reassigned AFTER UPDATE OF owner_id ON workers
WHEN OLD.owner_id <> NEW.owner_id
BEGIN
    DELETE FROM usage_workers WHERE worker_id=OLD.id;
    DELETE FROM usage_sessions WHERE worker_id=OLD.id;
    DELETE FROM usage_accounts WHERE worker_id=OLD.id;
END;
CREATE TRIGGER usage_session_removed AFTER DELETE ON usage_sessions
BEGIN
    DELETE FROM usage_members WHERE kind='session' AND member_id=OLD.id;
END;
CREATE TRIGGER usage_account_removed AFTER DELETE ON usage_accounts
WHEN NOT EXISTS (SELECT 1 FROM usage_accounts WHERE account_id=OLD.account_id)
BEGIN
    DELETE FROM usage_members WHERE kind='provider' AND member_id=OLD.account_id;
END;
-- One scheduling decision per provider account across all admitted Workers.
CREATE TABLE usage_collectors (
    account_id TEXT PRIMARY KEY,
    preferred_worker TEXT REFERENCES workers(id) ON DELETE SET NULL,
    preferred_session TEXT NOT NULL DEFAULT '',
    active_worker TEXT REFERENCES workers(id) ON DELETE SET NULL,
    lease_id TEXT NOT NULL DEFAULT '',
    lease_until INTEGER NOT NULL DEFAULT 0,
    next_due INTEGER NOT NULL DEFAULT 0,
    failures INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'waiting',
    last_probe INTEGER NOT NULL DEFAULT 0,
    hide_previous_schedule INTEGER NOT NULL DEFAULT 0,
    requested INTEGER NOT NULL DEFAULT 0,
    last_success INTEGER
);
CREATE INDEX usage_collectors_due ON usage_collectors(next_due);
CREATE TRIGGER usage_collector_orphan AFTER DELETE ON usage_accounts
WHEN NOT EXISTS (SELECT 1 FROM usage_accounts WHERE account_id=OLD.account_id)
BEGIN
    DELETE FROM usage_collectors WHERE account_id=OLD.account_id;
END;
ALTER TABLE usage_workers ADD COLUMN event_cursor INTEGER NOT NULL DEFAULT 0;
CREATE TABLE usage_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    worker_id TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    seq INTEGER,
    session_id TEXT NOT NULL,
    observed_at INTEGER NOT NULL,
    body TEXT NOT NULL,
    UNIQUE(worker_id,seq)
);
CREATE INDEX usage_events_time ON usage_events(observed_at);
CREATE TRIGGER usage_events_reassigned AFTER UPDATE OF owner_id ON workers
WHEN OLD.owner_id<>NEW.owner_id
BEGIN
    DELETE FROM usage_events WHERE worker_id=OLD.id;
END;
-- Per-viewer account nicknames and order are private, just like buckets.
CREATE TABLE usage_account_prefs (
    owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL,
    label TEXT NOT NULL,
    position INTEGER NOT NULL,
    PRIMARY KEY(owner_id,account_id)
) WITHOUT ROWID;
CREATE TRIGGER usage_account_prefs_orphan AFTER DELETE ON usage_accounts
WHEN NOT EXISTS (SELECT 1 FROM usage_accounts WHERE account_id=OLD.account_id)
BEGIN
    DELETE FROM usage_account_prefs WHERE account_id=OLD.account_id;
END;

-- The host reports exact start times where available. A vanished session is
-- an observation at scan time; the native stop hook has its own event.
CREATE TRIGGER usage_session_observed AFTER INSERT ON usage_sessions
BEGIN
    INSERT INTO usage_events(worker_id,session_id,observed_at,body)
    VALUES(NEW.worker_id,NEW.id,NEW.observed_at,json_object(
      'id',lower(hex(randomblob(32))),'session_id',NEW.id,
      'account_id',coalesce(json_extract(NEW.body,'$.account_id'),''),'kind','session_observed','source','session-host','observed_at',NEW.observed_at));
END;
CREATE TRIGGER usage_session_end_observed AFTER UPDATE OF ended_at ON usage_sessions
WHEN OLD.ended_at IS NULL AND NEW.ended_at IS NOT NULL
BEGIN
    INSERT INTO usage_events(worker_id,session_id,observed_at,body)
    VALUES(NEW.worker_id,NEW.id,NEW.ended_at,json_object(
      'id',lower(hex(randomblob(32))),'session_id',NEW.id,
      'account_id',coalesce(json_extract(NEW.body,'$.account_id'),''),'kind','session_end_observed','source','session-host','observed_at',NEW.ended_at));
END;
CREATE TRIGGER usage_session_folder_changed AFTER UPDATE OF body ON usage_sessions
WHEN json_extract(OLD.body,'$.folder') IS NOT json_extract(NEW.body,'$.folder')
 AND json_extract(NEW.body,'$.folder') IS NOT NULL
BEGIN
    INSERT INTO usage_events(worker_id,session_id,observed_at,body)
    VALUES(NEW.worker_id,NEW.id,NEW.observed_at,json_object(
      'id',lower(hex(randomblob(32))),'session_id',NEW.id,
      'account_id',coalesce(json_extract(NEW.body,'$.account_id'),''),'kind','folder_changed','source','session-host','observed_at',NEW.observed_at));
END;
CREATE TRIGGER usage_session_model_changed AFTER UPDATE OF body ON usage_sessions
WHEN json_extract(OLD.body,'$.model') IS NOT json_extract(NEW.body,'$.model')
 AND json_extract(NEW.body,'$.model') IS NOT NULL
BEGIN
    INSERT INTO usage_events(worker_id,session_id,observed_at,body)
    VALUES(NEW.worker_id,NEW.id,NEW.observed_at,json_object(
      'id',lower(hex(randomblob(32))),'session_id',NEW.id,
      'account_id',coalesce(json_extract(NEW.body,'$.account_id'),''),'kind','model_changed','source','native-model','observed_at',NEW.observed_at));
END;

-- Removing one of several sources cuts the old source's provenance. Keep a
-- provider cooldown in force internally without disclosing its historical time
-- to owners who can read only the remaining sources.
CREATE TRIGGER usage_collector_source_removed AFTER DELETE ON usage_accounts
BEGIN
    UPDATE usage_collectors SET active_worker=NULL,lease_id='',lease_until=0,
      last_success=NULL,hide_previous_schedule=1,
      status=CASE WHEN EXISTS(SELECT 1 FROM usage_accounts WHERE account_id=OLD.account_id AND can_poll=1) THEN 'waiting' ELSE 'passive' END
      WHERE account_id=OLD.account_id AND active_worker=OLD.worker_id;
    UPDATE usage_collectors SET preferred_worker=NULL,preferred_session=''
      WHERE account_id=OLD.account_id AND preferred_worker=OLD.worker_id;
END;

-- Run source cleanup before other cascades can null the provenance pointer.
CREATE TRIGGER usage_worker_accounts_before_delete BEFORE DELETE ON workers
BEGIN
    DELETE FROM usage_accounts WHERE worker_id=OLD.id;
END;
