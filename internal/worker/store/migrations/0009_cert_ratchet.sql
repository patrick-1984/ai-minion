-- Hub certificate ratchet (protocol 3.0, design hub-identity-v2 sections 6, 7
-- and 12.2). hub_enrollment gains the highest certificate serial this Worker has
-- seen under the pinned root and the SHA-256 of the certificate at that serial;
-- both belong to the pinned root and are reset when a verified transition
-- changes the pin. A pin from 0.6 has max_serial 0 and no hash.
ALTER TABLE hub_enrollment ADD COLUMN max_serial INTEGER NOT NULL DEFAULT 0 CHECK (max_serial >= 0);
ALTER TABLE hub_enrollment ADD COLUMN cert_sha256 BLOB CHECK (cert_sha256 IS NULL OR length(cert_sha256) = 32);

-- The pairing resume record: the pairing code, its public id and expiry, and the
-- authenticated candidate root with its certificate ratchet from the handshake
-- that authenticated the Hub. It is written durably before sig_worker is sent
-- and deleted in the transaction that commits the enrollment, at expiry and by
-- "minion worker reset". At most one row. The connect mode ignores it.
CREATE TABLE pair_resume (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    code           TEXT    NOT NULL,
    pairing_id     TEXT    NOT NULL,
    expires_at     INTEGER NOT NULL,
    root_pub       BLOB    NOT NULL CHECK (length(root_pub) = 32),
    chain_position INTEGER NOT NULL DEFAULT 0 CHECK (chain_position = 0),
    serial         INTEGER NOT NULL CHECK (serial >= 1),
    cert_sha256    BLOB    NOT NULL CHECK (length(cert_sha256) = 32)
);
