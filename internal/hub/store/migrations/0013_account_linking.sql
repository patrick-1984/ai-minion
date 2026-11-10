-- Story 6.4: linked accounts (AD-10, R67-R70). The auth domain owns these
-- tables. Time columns are unix milliseconds.

-- Linking policy of one OIDC provider. The key is the provider id as the
-- sign-in uses it, so it also covers the provider that the settings describe,
-- which has no row in oidc_providers; there is therefore no foreign key, and
-- the trigger below removes the policy of a provider deleted in the panel (a
-- provider created later under the same id must not inherit "linking allowed").
-- Every flag is off by default: no row means no linking and no auto-create.
CREATE TABLE oidc_link_policies (
    provider_id     TEXT    NOT NULL PRIMARY KEY,
    linking_allowed INTEGER NOT NULL DEFAULT 0 CHECK (linking_allowed IN (0, 1)),
    -- auto_create: when linking is refused, create a separate account instead
    -- of refusing the sign-in.
    auto_create     INTEGER NOT NULL DEFAULT 0 CHECK (auto_create IN (0, 1)),
    updated_at      INTEGER NOT NULL
);

-- The e-mail domains (lower case) whose addresses may be linked at a provider.
-- No row means no domain is allowed.
CREATE TABLE oidc_link_domains (
    provider_id TEXT NOT NULL REFERENCES oidc_link_policies (provider_id) ON DELETE CASCADE,
    domain      TEXT NOT NULL CHECK (domain <> ''),
    PRIMARY KEY (provider_id, domain)
);

CREATE TRIGGER oidc_link_policies_provider_deleted AFTER DELETE ON oidc_providers
BEGIN
    DELETE FROM oidc_link_policies WHERE provider_id = OLD.id;
END;

-- The panel consent of an admin account to be the target of an e-mail link
-- (off by default: no row). It is removed with the account.
CREATE TABLE admin_link_consent (
    user_id    TEXT    NOT NULL PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    granted_by TEXT    NOT NULL,
    granted_at INTEGER NOT NULL
);
