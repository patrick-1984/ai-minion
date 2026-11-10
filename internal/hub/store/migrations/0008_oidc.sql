-- Story 6.2: OIDC login with role rules (AD-10, AD-16). The auth domain owns
-- these tables. Time columns are unix milliseconds.

-- Providers configured in the panel. The provider that MINION_OIDC_* names is
-- not stored here: it comes from the settings layer and is read-only. The
-- client secret is AES-256-GCM sealed (AAD oidc_providers/client_secret/<id>);
-- a NULL value means the provider has no secret (a public client).
CREATE TABLE oidc_providers (
    id                       TEXT    NOT NULL PRIMARY KEY,           -- short slug, used in URLs
    name                     TEXT    NOT NULL,                       -- "Sign in with <name>"
    issuer                   TEXT    NOT NULL,
    client_id                TEXT    NOT NULL,
    client_secret_enc        BLOB,
    role_claim               TEXT    NOT NULL DEFAULT '',
    scopes                   TEXT    NOT NULL DEFAULT '',            -- space separated, '' = default
    ca_file                  TEXT    NOT NULL DEFAULT '',
    tls_insecure_skip_verify INTEGER NOT NULL DEFAULT 0 CHECK (tls_insecure_skip_verify IN (0, 1)),
    enabled                  INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL
);

-- Claim value rules: a user whose role claim holds one of the values of a
-- role gets that role (source idp). No matching rule means no access.
CREATE TABLE role_rules (
    provider_id TEXT NOT NULL REFERENCES oidc_providers (id) ON DELETE CASCADE,
    role        TEXT NOT NULL CHECK (role IN ('admin', 'user')),
    value       TEXT NOT NULL CHECK (value <> ''),
    PRIMARY KEY (provider_id, role, value)
);

-- An account at an IdP: identity is (issuer, subject), which is unique. The
-- refresh token is sealed (AAD external_identities/refresh_token/<id>).
CREATE TABLE external_identities (
    id                TEXT    NOT NULL PRIMARY KEY,                  -- UUIDv7
    user_id           TEXT    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    issuer            TEXT    NOT NULL,
    subject           TEXT    NOT NULL,
    provider_id       TEXT    NOT NULL DEFAULT '',
    refresh_token_enc BLOB,
    created_at        INTEGER NOT NULL,
    last_login_at     INTEGER NOT NULL,
    UNIQUE (issuer, subject)
);

CREATE INDEX external_identities_user ON external_identities (user_id);

-- One row per started sign-in, deleted when the callback consumes it or when
-- it expires. state_hash and binding_hash are SHA-256 (hex) of the random
-- state parameter and of the browser-binding cookie; the PKCE verifier is
-- sealed (AAD oidc_login_transactions/code_verifier/<state_hash>).
CREATE TABLE oidc_login_transactions (
    state_hash        TEXT    NOT NULL PRIMARY KEY,
    binding_hash      TEXT    NOT NULL,
    provider_id       TEXT    NOT NULL,
    nonce             TEXT    NOT NULL,
    code_verifier_enc BLOB    NOT NULL,
    next_path         TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    expires_at        INTEGER NOT NULL
);

CREATE INDEX oidc_login_transactions_expires ON oidc_login_transactions (expires_at);
