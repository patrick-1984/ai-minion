// Package authz decides whether a principal may perform an action on a resource, including trust (AD-10, AD-11, AD-17).
//
// Every authorisation in the Hub goes through one function,
// [Authorizer.Decide]. There is no second path: REST handlers, the op router
// and the SSE stream all call it, and list endpoints narrow their results with
// [Filter].
//
// The model in short:
//
//   - A [Principal] is a browser session, an API key, an act-as session or a
//     Worker identity (principal.go). It carries identifiers only, never a
//     role: the current role and activity of the user behind it are read from
//     the store through the [UserLookup] and [WorkerLookup] ports on every call, with no positive
//     cache, so a demoted admin or a deactivated user loses access on the next
//     request.
//   - A [Resource] is a user, a Worker or a session (resource.go). A session
//     belongs to the owner of its Worker (AD-17: one owner per Worker).
//   - The base permission of a user on a resource is: admin, everything
//     including manage; owner, read, operate and message; anyone else,
//     nothing. manage is global and admin only. The actions are alternatives
//     and never imply each other (ops.ActionSet.Permits).
//   - A principal with a [Scope] (an API key, added by Story 6.1) gets
//     scope ∩ base, so a key never outlives its owner's permissions. A key
//     bound to a Worker identity is accepted only on that Worker's call
//     streams (checked by authentication); it is not limited in resources.
//   - A Worker identity resolves its Worker's owner on every call and holds
//     read and message on the Workers and sessions of that owner, nothing on
//     user resources and never operate or manage. It denies when the Worker
//     is removed or the owner is missing or deactivated.
//   - [Authorizer.EffectiveActions] returns the set that goes into
//     op.request.actions (AD-12); the Worker makes the final message versus
//     operate check.
//
// Errors. Decide returns nil when allowed, otherwise a *proto.Error with the
// code forbidden or not_found. A principal that holds no action at all on a
// resource gets not_found, so that single-resource reads leak no existence; a
// principal that holds some action but not an accepted one gets forbidden.
// Failures of a lookup are returned as ordinary errors and always deny
// (fail closed).
//
// Wiring. The user port belongs to the auth module (Story 3.5) and the Worker
// port to the workers module (Story 4.3). The Hub module therefore provides an
// Authorizer from [NewUnwired], whose ports fail closed with [ErrNotWired], and
// those modules wire it once each with [Authorizer.SetUserLookup] and
// [Authorizer.SetWorkerLookup].
//
// Files: principal.go (principals and the Scope interface), resource.go
// (resources and the lookup ports), decide.go (Decide, EffectiveActions and
// wiring), filter.go (list filtering).
//
// Owner: Story 3.8.
package authz
