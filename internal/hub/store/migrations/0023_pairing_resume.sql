-- Pairing recovery (hub-identity-v2 section 12.2 and 12.3).
--
-- The pairing commit is recorded: CompletePairing writes the pairing id, the
-- Worker key, the Worker and the owner of the commit and the expiry of the
-- code in used_pairing_codes, in the same transaction that inserts the Worker.
-- A Worker whose welcome was lost resumes with the same code until
-- expires_at, with at most five wrong typed codes (resume_attempts). Rows
-- from before this migration have NULL pairing data and never resume (today's
-- behaviour). The stored digest is weak protection (about 20 unknown bits): a
-- resume relies on the same Worker key, the commit's owner, the expiry and the
-- attempt counter, never on the secrecy of the digest.
ALTER TABLE used_pairing_codes ADD COLUMN pairing_id      TEXT;
ALTER TABLE used_pairing_codes ADD COLUMN worker_pub      BLOB;
ALTER TABLE used_pairing_codes ADD COLUMN worker_id       TEXT;
ALTER TABLE used_pairing_codes ADD COLUMN owner_id        TEXT;
ALTER TABLE used_pairing_codes ADD COLUMN expires_at      INTEGER;
ALTER TABLE used_pairing_codes ADD COLUMN resume_attempts INTEGER NOT NULL DEFAULT 0;

-- first_connected_at is set when a connect handshake registers the Worker (NULL
-- until then). Every Worker that exists at the migration is backfilled with its
-- created_at, so no existing Worker is eligible for owner re-issue.
-- repair_until is the end of the owner's 10 minute re-issue window (0 = none).
ALTER TABLE workers ADD COLUMN first_connected_at INTEGER;
ALTER TABLE workers ADD COLUMN repair_until       INTEGER NOT NULL DEFAULT 0;
UPDATE workers SET first_connected_at = created_at;
