-- 0.7.0 (I2): two-tier Hub identity (docs/architecture/proposals/hub-identity-v2.md,
-- section 7). The singleton hub_identity row gains the certificate state, the
-- sealed-root record and the passphrase lockout and allowance state of the two
-- scopes (web and local command). The migration creates no certificate and no
-- key; the Hub service (internal/hub/hubid) writes those. All serials fit
-- SQLite's signed integer. Unix milliseconds throughout.

ALTER TABLE hub_identity ADD COLUMN last_issued INTEGER NOT NULL DEFAULT 0 CHECK (last_issued >= 0);
ALTER TABLE hub_identity ADD COLUMN current_serial INTEGER NOT NULL DEFAULT 0 CHECK (current_serial >= 0);
ALTER TABLE hub_identity ADD COLUMN current_cert BLOB CHECK (current_cert IS NULL OR length(current_cert) = 153);
ALTER TABLE hub_identity ADD COLUMN root_sealed INTEGER NOT NULL DEFAULT 0 CHECK (root_sealed IN (0, 1));
ALTER TABLE hub_identity ADD COLUMN root_file_sha256 BLOB CHECK (root_file_sha256 IS NULL OR length(root_file_sha256) = 32);
ALTER TABLE hub_identity ADD COLUMN migrated_from_06 INTEGER NOT NULL DEFAULT 0 CHECK (migrated_from_06 IN (0, 1));

-- Web scope: failure counter, lock end, and the hourly allowance window.
ALTER TABLE hub_identity ADD COLUMN unseal_failures INTEGER NOT NULL DEFAULT 0 CHECK (unseal_failures >= 0);
ALTER TABLE hub_identity ADD COLUMN unseal_locked_until INTEGER NOT NULL DEFAULT 0;
ALTER TABLE hub_identity ADD COLUMN unseal_window_start INTEGER NOT NULL DEFAULT 0;
ALTER TABLE hub_identity ADD COLUMN unseal_window_count INTEGER NOT NULL DEFAULT 0 CHECK (unseal_window_count >= 0);
-- Pending-attempt intent of each scope: set (with the allowance count) before
-- the key derivation, cleared by the failure record or by a right passphrase.
-- An intent found at start is an unsettled attempt and counts as a failure.
ALTER TABLE hub_identity ADD COLUMN unseal_pending INTEGER NOT NULL DEFAULT 0 CHECK (unseal_pending IN (0, 1));
ALTER TABLE hub_identity ADD COLUMN cli_unseal_pending INTEGER NOT NULL DEFAULT 0 CHECK (cli_unseal_pending IN (0, 1));
-- Local command scope: the same, independent of the web scope.
ALTER TABLE hub_identity ADD COLUMN cli_unseal_failures INTEGER NOT NULL DEFAULT 0 CHECK (cli_unseal_failures >= 0);
ALTER TABLE hub_identity ADD COLUMN cli_unseal_locked_until INTEGER NOT NULL DEFAULT 0;
ALTER TABLE hub_identity ADD COLUMN cli_window_start INTEGER NOT NULL DEFAULT 0;
ALTER TABLE hub_identity ADD COLUMN cli_window_count INTEGER NOT NULL DEFAULT 0 CHECK (cli_window_count >= 0);

-- Status page: the last successful identity operation.
ALTER TABLE hub_identity ADD COLUMN last_op TEXT NOT NULL DEFAULT '';
ALTER TABLE hub_identity ADD COLUMN last_op_at INTEGER NOT NULL DEFAULT 0;
-- Once-per-UTC-day warning event (days since the epoch) and the serial whose
-- expiry was already audited, so a restart does not repeat either event.
ALTER TABLE hub_identity ADD COLUMN expiry_warn_day INTEGER NOT NULL DEFAULT 0;
ALTER TABLE hub_identity ADD COLUMN expired_serial INTEGER NOT NULL DEFAULT 0;

-- An identity that exists now was created by 0.6: its root is the plain
-- identity.key (state B1) until the owner sets the passphrase, and the page
-- recommends one master-key rotation (section 14).
UPDATE hub_identity SET migrated_from_06 = 1;
