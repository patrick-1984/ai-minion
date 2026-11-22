-- Nominated usage readback (A79/A80). A nomination publishes one of the
-- owner's private buckets to Workers: scope 'user' to the owner's own Workers,
-- scope 'domain' to every Worker of the Hub. All columns are NOT NULL so the
-- composite foreign key is always enforced; deleting the bucket (or its user)
-- deletes the nomination.
--
-- owner_id is the publisher, the owner of the bucket. A user slot is unique per
-- (owner, kind); the domain has one slot per kind that any authorized
-- administrator replaces or clears as a whole, so the partial unique index is
-- the domain rule and the primary key only names the row.
CREATE TABLE usage_nominations (
    scope      TEXT    NOT NULL CHECK (scope IN ('user', 'domain')),
    owner_id   TEXT    NOT NULL CHECK (owner_id <> ''),
    kind       TEXT    NOT NULL CHECK (kind IN ('session', 'provider')),
    bucket_id  TEXT    NOT NULL CHECK (bucket_id <> ''),
    actor_id   TEXT    NOT NULL CHECK (actor_id <> ''),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (scope, owner_id, kind),
    FOREIGN KEY (owner_id, kind, bucket_id) REFERENCES usage_buckets (owner_id, kind, id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE UNIQUE INDEX usage_nominations_domain_kind ON usage_nominations (kind) WHERE scope = 'domain';
CREATE INDEX usage_nominations_bucket ON usage_nominations (bucket_id);

-- Permission to collect is not permission to republish. A source owner allows
-- the Hub to include observations of their sessions and accounts in buckets
-- published by someone else. The default (no row) is off.
CREATE TABLE usage_publication_consent (
    owner_id        TEXT    PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    allow_republish INTEGER NOT NULL DEFAULT 0 CHECK (allow_republish IN (0, 1)),
    updated_at      INTEGER NOT NULL
) WITHOUT ROWID;

-- A domain nomination lives only as long as its publisher is an active
-- administrator. Retiring it (not merely ignoring it) keeps a later
-- re-promotion from silently publishing the old choice again.
CREATE TRIGGER usage_nomination_admin_lost AFTER DELETE ON user_roles
WHEN OLD.role = 'admin' AND NOT EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = OLD.user_id AND r.role = 'admin')
BEGIN
    DELETE FROM usage_nominations WHERE scope = 'domain' AND owner_id = OLD.user_id;
END;
CREATE TRIGGER usage_nomination_deactivated AFTER UPDATE OF active ON users
WHEN NEW.active = 0
BEGIN
    DELETE FROM usage_nominations WHERE scope = 'domain' AND owner_id = NEW.id;
END;
