-- 0.7.0 (I4): the authentication time and method of a browser session
-- (docs/architecture/proposals/hub-identity-v2.md, section 8.1 item 2).
-- Both are written only by the server after a credential check it performed
-- itself. auth_at 0 means "no verified authentication": such a session is
-- never fresh. auth_method is 'local', 'oidc:<provider id>' or '' (unknown).
-- Sessions that exist at upgrade keep 0 and must re-authenticate before an
-- identity operation.
ALTER TABLE sessions ADD COLUMN auth_at INTEGER NOT NULL DEFAULT 0 CHECK (auth_at >= 0);
ALTER TABLE sessions ADD COLUMN auth_method TEXT NOT NULL DEFAULT '';
