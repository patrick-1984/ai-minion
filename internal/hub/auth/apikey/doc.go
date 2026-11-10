// Package apikey implements API keys and scope evaluation for the Hub (AD-11).
//
// A key is "mn_" plus 52 base32 characters (identity.APIKey). The Hub stores
// only its SHA-256 and an 8-character hint; the key text is returned once, by
// [Service.Create], and never again, and never reaches a log or an audit row.
//
// The package has two halves that share a [Service]:
//
//   - Lifecycle (apikey.go): Create, List and Revoke. Create writes the key,
//     its scope rows and the audit row in one transaction (AD-16). A caller can
//     grant only what they hold: every requested action must be among the
//     caller's current effective actions on every listed resource, which makes
//     "everything" and "manage" admin only and keeps a user inside their own
//     Workers and sessions. Keys cannot create, list or revoke keys: a leaked
//     key must not mint a stronger one.
//
//   - Authentication (apikey.go) and scope evaluation (scope.go).
//     [Service.AuthenticateKey] reads the store on every request (no cache),
//     refuses an unknown, expired or revoked key and one whose owner is
//     deactivated, and returns an authz principal that carries a [Scope]. The
//     [Scope] says which actions the key grants on a resource, and
//     authz.Decide intersects that with the owner's current permissions on every
//     decision, so demoting the owner shrinks the key at once.
//
// A key bound to a Worker identity (bound_worker_id) is accepted only on the
// call streams of that Worker: [Service.AuthenticateCall] and
// [CheckCallBinding] are the check the call-stream handler of Story 7.4 uses,
// and the REST authenticator (hub/api) refuses a bound key outright. Expiry
// applies to keys only; an enrolled Worker never expires.
//
// The package reaches the database through the [Repository] port, which
// *store.Store implements, and authorisation through [Authorizer], so the logic
// is unit-tested with fakes. The Hub module that wires it is
// hub/server/mod_apikey.go and the REST handlers are hub/api/h_apikeys.go.
//
// Owner: Story 6.1.
package apikey
