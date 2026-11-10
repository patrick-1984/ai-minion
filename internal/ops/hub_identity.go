package ops

import "reflect"

// OpHubIdentityRotate is the name of the Hub operation that rotates the Hub
// identity key (AD-5, R94).
const OpHubIdentityRotate = "hub_identity_rotate"

// Warning codes of hub_identity_rotate.
const (
	// WarnInvitationsRevoked says the rotation revoked the outstanding
	// invitation keys: each names the fingerprint of the Hub key it was
	// created under, so new ones must be created.
	WarnInvitationsRevoked = "invitations_revoked"
	// WarnRotationFollowUp says the rotation is committed and active but a
	// step after the commit failed (replacing identity.key, revoking the
	// invitation keys created meanwhile). The Hub log has the details; an
	// interrupted file replacement is completed at the next start.
	WarnRotationFollowUp = "rotation_follow_up_failed"
)

var (
	// guardIdentityChainNotFull refuses a rotation when the transition chain
	// has reached its size limit. It is KEEP: force does not override it, the
	// remedy is a new Hub identity with re-enrolled Workers (AD-5).
	guardIdentityChainNotFull = Guard{Name: "hub_identity_chain_not_full", Class: GuardKeep, Reads: "hub_identity.chain_length"}
	// guardIdentityInvitations reports that outstanding invitation keys are
	// revoked by the rotation. WARN: the rotation goes ahead.
	guardIdentityInvitations = Guard{Name: "hub_identity_invitations_revoked", Class: GuardWarn, Reads: "invitations.outstanding"}
)

// HubIdentityRotateArgs are the arguments of hub_identity_rotate. The
// rotation creates a new Hub identity key, appends a transition record signed
// by the old key and revokes every outstanding invitation key. Workers pinned
// to the old key follow the chain on their next connect; none is re-paired.
type HubIdentityRotateArgs struct {
	WriteArgs
}

// Validate checks the request id.
func (a HubIdentityRotateArgs) Validate() error { return a.WriteArgs.Validate() }

func init() {
	Register(Op{
		Name: OpHubIdentityRotate, Summary: "Rotate the Hub identity key: append a signed transition, revoke the outstanding invitation keys; Workers follow the chain without re-pairing.",
		Scope: ScopeHub, Actions: MustActionSet(ActionManage), Write: true,
		Guards: []Guard{guardIdentityChainNotFull, guardIdentityInvitations},
		Args:   reflect.TypeFor[HubIdentityRotateArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
