# Sign in with OIDC

Back to the [README](../../README.md).

The Hub can sign users in through an OpenID Connect identity provider (IdP) and give them roles from a claim in the ID token. Local accounts keep working next to it: the first administrator is always a local account, so you can fix the IdP settings if they are wrong.

## What the Hub needs from the IdP

- A **confidential client** that uses the authorization code flow. The Hub uses PKCE (S256), `state` and `nonce`.
- The **redirect URI**: the Hub's public URL plus `/auth/oidc/callback`, for example `https://hub.example.com/auth/oidc/callback`. Behind a reverse proxy the public URL is `MINION_PUBLIC_URL` ([reverse proxy](reverse-proxy.md)); register exactly that address.
- A **claim that carries the roles**, usually a group list such as `groups`, in the ID token (not only in the userinfo response). Many IdPs release group claims only when a scope asks for them; add that scope to `MINION_OIDC_SCOPES` (the default is `openid profile email`).
- Two groups, or two claim values, one for administrators and one for users.
- **A refresh token, if you want roles to be re-checked in the background.** The Hub does not ask for one by itself: it sends only the scopes in `MINION_OIDC_SCOPES`. Many IdPs issue a refresh token to a confidential client only when the scope `offline_access` is requested (and allowed for that client, sometimes with a consent step); others issue one without it. Look in your IdP's documentation for how it issues refresh tokens, enable that for the Minion client, and add `offline_access` to `MINION_OIDC_SCOPES` when the IdP requires it. See [Background role updates](#background-role-updates).

## Configure the Hub

OIDC turns on when an issuer is set. With environment variables (one provider):

```sh
MINION_OIDC_NAME=SSO
MINION_OIDC_ISSUER=https://idp.example.com/application/o/minion/
MINION_OIDC_CLIENT_ID=minion
MINION_OIDC_CLIENT_SECRET_FILE=/run/secrets/minion_oidc_client_secret
MINION_OIDC_ROLE_CLAIM=groups
MINION_OIDC_ADMIN_VALUES=minion-admins
MINION_OIDC_USER_VALUES=minion-users
MINION_OIDC_SCOPES="openid profile email groups offline_access"
```

| Variable | Meaning |
| --- | --- |
| `MINION_OIDC_ISSUER` | Issuer URL of the IdP. The Hub fetches its discovery document from it. |
| `MINION_OIDC_CLIENT_ID` | The client id. |
| `MINION_OIDC_CLIENT_SECRET_FILE` | A file with the client secret. A secret is never taken from the command line; `MINION_OIDC_CLIENT_SECRET` also exists, but a file keeps it out of the process environment. |
| `MINION_OIDC_ROLE_CLAIM` | The ID token claim that carries the roles, for example `groups`. |
| `MINION_OIDC_ADMIN_VALUES`, `MINION_OIDC_USER_VALUES` | Comma-separated claim values that give the administrator role and the user role. |
| `MINION_OIDC_NAME` | The name on the login page ("Sign in with SSO"). |
| `MINION_OIDC_SCOPES` | Space-separated scopes. Add `offline_access` where your IdP needs it to issue a refresh token. |
| `MINION_OIDC_TIMEOUT` | Deadline of every call to the IdP, a Go duration such as `10s` (1s to 2m). |
| `MINION_OIDC_CA_FILE` | A PEM file with the private CA that signed the IdP's certificate. Prefer it to the next line. |
| `MINION_OIDC_TLS_INSECURE_SKIP_VERIFY` | Do not verify the IdP's TLS certificate. Insecure: the Hub shows a permanent warning to administrators while it is on. |
| `MINION_OIDC_REFRESH_INTERVAL` | How often the Hub re-checks the roles of OIDC accounts that have a refresh token, default `24h` (1m to 720h). |

Restart the Hub after you change environment variables. The same settings, and more than one provider, are managed in the Hub as an administrator: **Admin**, **OIDC**, or from the command line with `minion oidc_provider_set` and `minion role_rule_set` against the Hub ([API keys and MCP](api-keys-and-mcp.md)). A setting that is set in the environment is shown as locked in the panel.

## Roles fail closed

- The Hub reads **only** the configured claim. A claim that is missing, empty or malformed gives no role, and a claim whose values match neither list gives no role. **No role means no access**: the sign-in is refused. A user whose group is not listed cannot get in.
- The claim may be one string or a list of strings; other values are ignored.
- Roles are set at each sign-in from the ID token of that sign-in. Between sign-ins they change only as [described below](#background-role-updates).
- Local grants and Worker pairings are never changed by the IdP.

## Background role updates

Removing a person from the group at the IdP takes effect in the Hub in one of two ways, and only the first is automatic:

- **With a stored refresh token:** the Hub re-checks the roles of every OIDC account that holds a refresh token, once per `MINION_OIDC_REFRESH_INTERVAL` (24 hours unless you set it; not instantly). It exchanges the refresh token and reads the roles from the fresh ID token the IdP returns, so the IdP must return an ID token with the role claim on a refresh. If the IdP permanently rejects the token, the person's IdP roles are suspended and their browser sessions end until they sign in again. An IdP that cannot be reached changes nothing, and the next pass tries again.
- **Without a refresh token:** an account whose sign-in did not return a refresh token is **not** refreshed in the background. Its roles are renewed only at its next sign-in, so a removal at the IdP shows up only then.

When access must end **at once**, or for an account without a refresh token, do not rely on the IdP: deactivate the user in the Hub (**Admin**, **Users**, or `minion user_deactivate`). Do not assume background updates are on because OIDC sign-in works: sign-in succeeds without a refresh token.

Test it with your IdP before you depend on it: sign in a test person who is in the user group, remove them from that group at the IdP, wait for the refresh interval (set `MINION_OIDC_REFRESH_INTERVAL=1m` for the test, and restart the Hub), and check that their access ends. If it does not, the IdP did not issue a refresh token for the client: check the client's settings and `offline_access`, and sign in again, because only a sign-in stores a token.

## Linking to existing accounts

An account made by an IdP sign-in is identified by the pair (issuer, subject), not by name or e-mail address. By default a first sign-in never attaches to an existing local account by e-mail address. An administrator can allow linking by e-mail address for a provider, for chosen domains, with `oidc_link_policy_set` (off by default), and can detach an identity again with `identity_detach`. `admin_link_consent` controls whether an e-mail link into an administrator account is allowed at all; it is off by default.

## Check it

Open the Hub's login page: it shows "Sign in with SSO" (or the name you set). Sign in with a person who is in the administrator group, and with one who is in no listed group: the second must be refused. A failed sign-in ends on an English error page, and the attempt is recorded in the audit log (**Admin**, **Audit**); neither contains a secret.
