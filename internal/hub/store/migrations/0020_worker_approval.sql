-- F-02 (v0.6): enrollment is not authorization.
--
-- A Worker that enrolls (invitation key, pairing code or the infrastructure key
-- file) starts "pending": it may connect, but the Hub routes no operation to
-- it and refuses its call-stream requests (except retiring itself) until the
-- owner or an admin approves it. The column defaults to 'pending', so a row
-- inserted without saying anything stays closed; the Hub's own insert states
-- the value explicitly.
ALTER TABLE workers ADD COLUMN approval TEXT NOT NULL DEFAULT 'pending'
    CHECK (approval IN ('pending', 'approved'));
ALTER TABLE workers ADD COLUMN enroll_addr TEXT NOT NULL DEFAULT '';
ALTER TABLE workers ADD COLUMN approved_at INTEGER;
ALTER TABLE workers ADD COLUMN approved_by TEXT NOT NULL DEFAULT '';

-- Every Worker that exists at upgrade time was enrolled under the old rules and
-- keeps working: it is approved by the migration.
UPDATE workers
   SET approval = 'approved',
       approved_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000,
       approved_by = 'migration';

-- Invitation keys. source 'infra_file' marks the row of the Hub-wide key file
-- (MINION_INFRA_INVITE_KEY_FILE): keyed by the hash of its secret, with expiry,
-- use counter and sticky revocation like any other key. It enrolls only while
-- the file currently holds that secret. notice is the owner-visible text the
-- migration leaves on a key it limited.
ALTER TABLE invitation_keys ADD COLUMN source TEXT NOT NULL DEFAULT 'user'
    CHECK (source IN ('user', 'infra_file'));
ALTER TABLE invitation_keys ADD COLUMN notice TEXT NOT NULL DEFAULT '';

-- Keys of non-admins that were created under the old rules get the new caps.
-- An unlimited key expires seven days after the upgrade.
UPDATE invitation_keys
   SET expires_at = (CAST(strftime('%s', 'now') AS INTEGER) + 7 * 24 * 3600) * 1000,
       notice = 'Limited by the upgrade to 0.6: this key had no expiry; it now expires 7 days after the upgrade.'
 WHERE revoked = 0
   AND expires_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = invitation_keys.creator_id AND r.role = 'admin');

-- A reusable key without a use limit may enroll ten more Workers.
UPDATE invitation_keys
   SET max_uses = uses + 10,
       notice = CASE WHEN notice = '' THEN '' ELSE notice || ' ' END ||
                'Limited by the upgrade to 0.6: this reusable key had no use limit; it now allows 10 more enrollments.'
 WHERE revoked = 0
   AND mode = 'reusable'
   AND max_uses IS NULL
   AND NOT EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = invitation_keys.creator_id AND r.role = 'admin');
