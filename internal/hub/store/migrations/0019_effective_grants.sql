-- F-07 (v0.6.0): an account without effective grants has no access.
--
-- Until v0.5 an account without any user_roles row resolved to the role
-- "user". From v0.6 it resolves to no role at all, so that losing every IdP
-- grant makes the account's API keys and Worker identities inert while the
-- account itself stays (and regains access at the next valid sign-in).
--
-- Step 1: every account that signs in locally gets an explicit local "user"
-- grant when it has none, so local users keep exactly the access they had:
--  * an account with a password (a v0.5 SetFirstPassword added passwords to
--    IdP accounts without a local grant, so IdP rows alone do not count), and
--  * an account without any role row and without an external identity.
-- An IdP-only account (no password) without any role row is one an IdP
-- offboarded: it stays without grants. Re-running the step changes nothing.
INSERT INTO user_roles (user_id, role, source)
SELECT u.id, 'user', 'local'
FROM users u
WHERE NOT EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.source = 'local')
  AND (u.password_hash <> ''
       OR (NOT EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id)
           AND NOT EXISTS (SELECT 1 FROM external_identities e WHERE e.user_id = u.id)));

-- Step 2: an IdP admin grant is re-checked only through a refresh token. An
-- existing source-idp admin grant whose account has no stored refresh token
-- can never be revoked by the background refresh, so it is suspended: the
-- grant row is removed, local grants stay, and an audit event records each
-- suspension. Recovery: the user signs in again; the grant returns only when
-- the IdP issues a refresh token at that sign-in.
INSERT OR IGNORE INTO audit_events (id, ts, actor, acting_as, action, target, outcome)
SELECT 'idp-admin-suspend-' || ur.user_id,
       CAST(strftime('%s', 'now') AS INTEGER) * 1000,
       'system', '', 'role.suspend', ur.user_id, 'changed'
FROM user_roles ur
WHERE ur.source = 'idp' AND ur.role = 'admin'
  AND NOT EXISTS (SELECT 1 FROM external_identities e
                  WHERE e.user_id = ur.user_id AND e.refresh_token_enc IS NOT NULL AND length(e.refresh_token_enc) > 0);

DELETE FROM user_roles
WHERE source = 'idp' AND role = 'admin'
  AND NOT EXISTS (SELECT 1 FROM external_identities e
                  WHERE e.user_id = user_roles.user_id AND e.refresh_token_enc IS NOT NULL AND length(e.refresh_token_enc) > 0);

-- The Hub also checks at every start that each remaining IdP admin grant has a
-- refresh token that decrypts to a non-empty value (SQL cannot decrypt); see
-- Store.suspendUnusableIdPAdmins.
