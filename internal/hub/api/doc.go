// Package api serves the REST API of the Hub from the operation catalog (AD-18, AD-2).
//
// One framework gives every route the same authentication, authorisation,
// error shape and time format. The package is split by concern:
//
//   - errors.go: the AD-18 error envelope {"error":{"code","message","details"}},
//     the [Error] type and the single mapping from a Go error to a code. Codes
//     come from internal/proto/codes.go and the status from proto.HTTPStatus.
//   - json.go: request decoding (unknown fields are ignored, AD-4), the query
//     string form of GET arguments, JSON responses and the RFC 3339 UTC
//     millisecond time format.
//   - auth.go: the [Authenticator]: a browser session cookie with CSRF, or
//     "Authorization: Bearer mn_..." through the [KeyAuthenticator] port. A
//     bearer token that fails is counted by the apikey_fail limiter per IP and
//     audited without the token.
//   - handlers.go: the handler [Registry]. A module registers the function that
//     executes a catalog operation with [Handle] (or [Registry.Handle]).
//   - dispatch.go: the [Dispatcher]. For a catalog operation with a registered
//     handler it authenticates, decodes and validates the arguments, derives
//     the resource, calls authz.Decide with the operation's accepted action
//     set and only then runs the handler. An operation without a handler
//     answers unsupported_op.
//   - router.go: the [Router] mounts POST /api/v1/ops/<name> (and GET for read
//     operations, with the arguments in the query string), resource aliases
//     such as GET /api/v1/audit, and an enveloped not_found for every other
//     /api/v1 path.
//
// Authorisation has one path: authz.Decide, with the accepted action set that
// the catalog declares for the operation. The resource it is decided on comes
// from the arguments (a Target or a worker_id names a session or a Worker; a
// Hub operation that names neither is decided on the caller's own user
// resource, a Worker operation that names no Worker is refused as
// invalid_argument), or from a [ResourceFunc] given at registration. A stream
// authenticates its request again every ReauthInterval and ends when the
// credentials or the permission are gone.
//
// The package is independent of the Hub kernel: everything it needs from the
// rest of the Hub is a small port (SessionResolver, KeyAuthenticator, Decider,
// Auditor, Limiter), so the logic is unit-tested with fakes. The Hub module
// that wires the ports is internal/hub/server/mod_api.go.
//
// Owner: Story 3.9.
package api
