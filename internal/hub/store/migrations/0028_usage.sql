-- Per-Worker attributes and latest successful full session observation.
CREATE TABLE usage_workers (
    worker_id TEXT PRIMARY KEY REFERENCES workers(id) ON DELETE CASCADE,
    observed_at INTEGER NOT NULL,
    complete_at INTEGER NOT NULL DEFAULT 0,
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
    observed_at INTEGER NOT NULL,
    counters TEXT NOT NULL,
    PRIMARY KEY (session_id, hour)
) WITHOUT ROWID;
CREATE INDEX usage_hours_age ON usage_hours(hour);
CREATE TABLE usage_accounts (
    worker_id TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL,
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
