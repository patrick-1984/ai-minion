package ops

import (
	"encoding/json"
	"reflect"
	"time"
	"unicode"
	"unicode/utf8"
)

// Names of the invitation operations (AD-2, AD-7).
const (
	OpInvitationCreate = "invitation_create"
	OpInvitationList   = "invitation_list"
	OpInvitationRevoke = "invitation_revoke"
)

// Invitation modes: a one-time key enrolls exactly one Worker, a reusable key
// any number until it expires or is revoked (R91).
const (
	InvitationOneTime  = "one_time"
	InvitationReusable = "reusable"
)

// Worker kinds an invitation key can create (AD-17). An infrastructure key
// needs an admin.
const (
	KindPrivate        = "private"
	KindInfrastructure = "infrastructure"
)

// Invitation TTL labels (R91). The key expires that long after creation;
// unlimited never expires. Expiry stops new enrollments only: a Worker that
// has enrolled is never affected.
const (
	TTL1h        = "1h"
	TTL4h        = "4h"
	TTL8h        = "8h"
	TTL24h       = "24h"
	TTL1w        = "1w"
	TTL1m        = "1m"
	TTLUnlimited = "unlimited"
)

// InvitationTTLs lists the accepted TTL labels in ascending order.
func InvitationTTLs() []string {
	return []string{TTL1h, TTL4h, TTL8h, TTL24h, TTL1w, TTL1m, TTLUnlimited}
}

// InvitationTTLDuration returns the lifetime of a TTL label. A month is 30
// days. ok is false for an unknown label; the duration is 0 for unlimited
// (with ok true).
func InvitationTTLDuration(ttl string) (d time.Duration, ok bool) {
	switch ttl {
	case TTL1h:
		return time.Hour, true
	case TTL4h:
		return 4 * time.Hour, true
	case TTL8h:
		return 8 * time.Hour, true
	case TTL24h:
		return 24 * time.Hour, true
	case TTL1w:
		return 7 * 24 * time.Hour, true
	case TTL1m:
		return 30 * 24 * time.Hour, true
	case TTLUnlimited:
		return 0, true
	}
	return 0, false
}

// Invitation states reported by invitation_list.
const (
	InvitationActive    = "active"
	InvitationExpired   = "expired"
	InvitationExhausted = "exhausted"
	InvitationRevoked   = "revoked"
)

// Caps of the invitation keys of non-admins (F-02). A non-admin cannot create
// a key that is valid longer than NonAdminMaxInvitationTTL (so no "1m" and no
// "unlimited"), and a reusable key needs a use limit: DefaultReusableMaxUses
// when none is given, at most MaxReusableUsesNonAdmin. Admins are not capped;
// an admin's reusable key without max_uses stays unlimited.
const (
	NonAdminMaxInvitationTTL = TTL1w
	DefaultReusableMaxUses   = 10
	MaxReusableUsesNonAdmin  = 100
	// MaxInvitationUses is the sanity bound of max_uses for everyone.
	MaxInvitationUses = 1000000
)

// MaxInvitationNameLen caps an invitation name in characters.
const MaxInvitationNameLen = 64

// InvitationCreateArgs are the arguments of invitation_create.
type InvitationCreateArgs struct {
	// Mode is one_time or reusable.
	Mode string `json:"mode"`
	// TTL is 1h, 4h, 8h, 24h, 1w, 1m or unlimited.
	TTL string `json:"ttl"`
	// Name is a label for the list; optional.
	Name string `json:"name,omitempty"`
	// Kind is private (the default) or infrastructure (admin only).
	Kind string `json:"kind,omitempty"`
	// MaxUses limits a reusable key to that many enrollments. A non-admin gets
	// DefaultReusableMaxUses when it is omitted and at most
	// MaxReusableUsesNonAdmin; an admin's reusable key without it has no
	// limit. A one-time key allows exactly one enrollment, so MaxUses is 0 or
	// 1 for it.
	MaxUses int `json:"max_uses,omitempty"`
}

// EffectiveKind returns Kind or private when it is empty.
func (a InvitationCreateArgs) EffectiveKind() string {
	if a.Kind == "" {
		return KindPrivate
	}
	return a.Kind
}

// Validate checks the mode, the TTL, the name and the kind. Whether the caller
// may create an infrastructure key is decided on the Hub.
func (a InvitationCreateArgs) Validate() error {
	if a.Mode != InvitationOneTime && a.Mode != InvitationReusable {
		return invalid("mode", "must be %s or %s", InvitationOneTime, InvitationReusable)
	}
	if _, ok := InvitationTTLDuration(a.TTL); !ok {
		return invalid("ttl", "must be one of 1h, 4h, 8h, 24h, 1w, 1m, unlimited")
	}
	if !validInvitationName(a.Name) {
		return invalid("name", "must be valid UTF-8 of at most %d characters without control characters", MaxInvitationNameLen)
	}
	if k := a.EffectiveKind(); k != KindPrivate && k != KindInfrastructure {
		return invalid("kind", "must be %s or %s", KindPrivate, KindInfrastructure)
	}
	if a.MaxUses < 0 || a.MaxUses > MaxInvitationUses {
		return invalid("max_uses", "must be between 1 and %d", MaxInvitationUses)
	}
	if a.Mode == InvitationOneTime && a.MaxUses > 1 {
		return invalid("max_uses", "a one-time key allows one enrollment; use a reusable key for more")
	}
	return nil
}

func validInvitationName(s string) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > MaxInvitationNameLen {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// InvitationCreateResult is the result of invitation_create. Key is the
// complete invitation key mnk_<hub fingerprint>.<secret>; it is shown only
// here and cannot be read again. The Hub stores only the hash of its secret.
type InvitationCreateResult struct {
	ID        string     `json:"id"`
	Key       string     `json:"key"`
	Name      string     `json:"name"`
	Mode      string     `json:"mode"`
	Kind      string     `json:"kind"`
	TTL       string     `json:"ttl"`
	CreatedAt Timestamp  `json:"created_at"`
	ExpiresAt *Timestamp `json:"expires_at"` // null for ttl unlimited
	// MaxUses is the use limit of the new key: 1 for a one-time key, null
	// for an admin's reusable key without a limit.
	MaxUses *int `json:"max_uses"`
}

// InvitationListArgs are the arguments of invitation_list.
type InvitationListArgs struct {
	// All lists the keys of every user instead of the caller's own. Admin
	// only.
	All bool `json:"all,omitempty"`
}

// InvitationInfo describes one invitation key. It never carries the secret or
// its hash.
type InvitationInfo struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Mode      string     `json:"mode"`
	Kind      string     `json:"kind"`
	TTL       string     `json:"ttl"`
	CreatorID string     `json:"creator_id"`
	CreatedAt Timestamp  `json:"created_at"`
	ExpiresAt *Timestamp `json:"expires_at"` // null for ttl unlimited
	// Uses is the use counter; MaxUses is 1 for a one-time key and null for
	// a reusable one.
	Uses      int        `json:"uses"`
	MaxUses   *int       `json:"max_uses"`
	Revoked   bool       `json:"revoked"`
	RevokedAt *Timestamp `json:"revoked_at"`
	// Status is active, expired, exhausted or revoked (checked in that
	// order of precedence: revoked first).
	Status string `json:"status"`
	// Source is "infra_file" for the row of the Hub-wide key file
	// (MINION_INFRA_INVITE_KEY_FILE) and omitted for a key made by a user.
	Source string `json:"source,omitempty"`
	// Notice is the text the upgrade to 0.6 left on a key it limited
	// (expiry or use limit added); empty otherwise.
	Notice string `json:"notice,omitempty"`
}

// InvitationListResult is the result of invitation_list, newest first.
type InvitationListResult struct {
	Invitations []InvitationInfo `json:"invitations"`
}

// MarshalJSON writes invitations as an empty array instead of null.
func (r InvitationListResult) MarshalJSON() ([]byte, error) {
	type plain InvitationListResult
	if r.Invitations == nil {
		r.Invitations = []InvitationInfo{}
	}
	return json.Marshal(plain(r))
}

// InvitationRevokeArgs are the arguments of invitation_revoke.
type InvitationRevokeArgs struct {
	WriteArgs
	// ID is the invitation id from invitation_create or invitation_list.
	ID string `json:"id"`
}

// Validate checks the request id and the invitation id.
func (a InvitationRevokeArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !ValidUUIDv7(a.ID) {
		return invalid("id", "must be a UUIDv7")
	}
	return nil
}

// guardInvitationOwned limits revocation to the creator of the key and to
// admins. It is KEEP: force does not override it.
var guardInvitationOwned = Guard{Name: "invitation_owned", Class: GuardKeep, Reads: "invitation.creator"}

func init() {
	// The accepted action is operate on the caller's own user resource: the
	// owner and admins hold it, so every user manages their own keys (R91).
	// Creating an infrastructure key and listing every user's keys are
	// admin-only and decided by the handler.
	operate := MustActionSet(ActionOperate)

	// invitation_create changes state but returns the new key, which does not
	// fit WriteResult, so the catalog cannot mark it as a write op; it is
	// marked Mutates instead (POST only, so a cross-site GET cannot create a
	// key without the CSRF check) and the service audits it itself.
	Register(Op{
		Name: OpInvitationCreate, Summary: "Create an invitation key for enrolling Workers; the key is shown once.",
		Scope: ScopeHub, Actions: operate, Mutates: true,
		Args: reflect.TypeFor[InvitationCreateArgs](), Result: reflect.TypeFor[InvitationCreateResult](),
	})
	Register(Op{
		Name: OpInvitationList, Summary: "List invitation keys with uses, expiry and state.",
		Scope: ScopeHub, Actions: MustActionSet(ActionRead),
		Args: reflect.TypeFor[InvitationListArgs](), Result: reflect.TypeFor[InvitationListResult](),
	})
	Register(Op{
		Name: OpInvitationRevoke, Summary: "Revoke an invitation key so that no further Worker can enroll with it.",
		Scope: ScopeHub, Actions: operate, Write: true,
		Guards: []Guard{guardInvitationOwned},
		Args:   reflect.TypeFor[InvitationRevokeArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
