package ops

import (
	"reflect"
	"unicode/utf8"
)

// Names of the Hub identity operations (docs/architecture/proposals/
// hub-identity-v2.md, section 8).
const (
	// OpHubIdentityStatus reads the state of the Hub identity.
	OpHubIdentityStatus = "hub_identity_status"
	// OpHubIdentitySetPassphrase seals the root under a first passphrase and
	// issues the first certificate (bootstrap states B0 and B1 only).
	OpHubIdentitySetPassphrase = "hub_identity_set_passphrase"
	// OpHubIdentityReissue issues a new operational key and certificate under
	// the same root.
	OpHubIdentityReissue = "hub_identity_reissue"
	// OpHubIdentityRotate rotates the master key (the root).
	OpHubIdentityRotate = "hub_identity_rotate"
	// OpHubIdentityChangePassphrase seals the same root under a new passphrase.
	OpHubIdentityChangePassphrase = "hub_identity_change_passphrase"
)

// Warning codes of the Hub identity operations.
const (
	// WarnInvitationsRevoked says the master key rotation revoked the
	// outstanding invitation keys: each names the fingerprint of the Hub key
	// it was created under, so new ones must be created.
	WarnInvitationsRevoked = "invitations_revoked"
)

// The Hub identity wire sizes. They bound the request, not the passphrase
// policy, which the service decides (and answers without the length).
const (
	maxIdentityPassphraseBytes = 1024
	// RotateConfirmWord is what the administrator types to confirm a master
	// key rotation.
	RotateConfirmWord = "ROTATE"
)

func checkPassphraseField(field, v string, required bool) error {
	switch {
	case required && v == "":
		return invalid(field, "is required")
	case len(v) > maxIdentityPassphraseBytes:
		return invalid(field, "is too long")
	case !utf8.ValidString(v):
		return invalid(field, "must be valid UTF-8")
	}
	return nil
}

// IssueMembers are the per-issuance options shared by the operations that
// issue a certificate.
type IssueMembers struct {
	// ValidityDays is 1 to 400; 0 means 180.
	ValidityDays int `json:"validity_days,omitempty"`
	// SerialFloor: the new serial must be greater than it (0 = none).
	SerialFloor int64 `json:"serial_floor,omitempty"`
	// AllowFarFloor allows a floor more than 366 days ahead.
	AllowFarFloor bool `json:"allow_far_floor,omitempty"`
}

func (m IssueMembers) validate() error {
	if m.ValidityDays < 0 || m.ValidityDays > 400 {
		return invalid("validity_days", "must be 1 to 400")
	}
	if m.SerialFloor < 0 {
		return invalid("serial_floor", "must not be negative")
	}
	return nil
}

const redactedIdentityArgs = "[hub identity arguments: passphrases redacted]"

// HubIdentitySetPassphraseArgs are the arguments of hub_identity_set_passphrase.
// The passphrase is in the request body only: never in a URL or header, and
// never printed (the String methods redact it).
type HubIdentitySetPassphraseArgs struct {
	Passphrase string `json:"passphrase"`
	Confirm    string `json:"confirm"`
	IssueMembers
}

// Validate checks sizes and encodings; the passphrase rules are the service's.
func (a HubIdentitySetPassphraseArgs) Validate() error {
	if err := checkPassphraseField("passphrase", a.Passphrase, true); err != nil {
		return err
	}
	if err := checkPassphraseField("confirm", a.Confirm, true); err != nil {
		return err
	}
	return a.validate()
}

// String redacts the passphrases.
func (HubIdentitySetPassphraseArgs) String() string { return redactedIdentityArgs }

// GoString redacts the passphrases.
func (HubIdentitySetPassphraseArgs) GoString() string { return redactedIdentityArgs }

// HubIdentityReissueArgs are the arguments of hub_identity_reissue.
type HubIdentityReissueArgs struct {
	Passphrase string `json:"passphrase"`
	IssueMembers
}

// Validate checks sizes and encodings.
func (a HubIdentityReissueArgs) Validate() error {
	if err := checkPassphraseField("passphrase", a.Passphrase, true); err != nil {
		return err
	}
	return a.validate()
}

// String redacts the passphrase.
func (HubIdentityReissueArgs) String() string { return redactedIdentityArgs }

// GoString redacts the passphrase.
func (HubIdentityReissueArgs) GoString() string { return redactedIdentityArgs }

// HubIdentityRotateArgs are the arguments of hub_identity_rotate: a new root
// sealed under a new passphrase, the transition signed by the old root, a new
// operational key and certificate, all invitations revoked. Workers pinned to
// the old key follow the chain at their next connect; none is re-paired.
type HubIdentityRotateArgs struct {
	// Passphrase is the current passphrase.
	Passphrase    string `json:"passphrase"`
	NewPassphrase string `json:"new_passphrase"`
	Confirm       string `json:"confirm"`
	// Word must be ROTATE.
	Word string `json:"word"`
	IssueMembers
}

// Validate checks sizes, encodings and the confirmation word.
func (a HubIdentityRotateArgs) Validate() error {
	for _, f := range []struct {
		name, v string
	}{{"passphrase", a.Passphrase}, {"new_passphrase", a.NewPassphrase}, {"confirm", a.Confirm}} {
		if err := checkPassphraseField(f.name, f.v, true); err != nil {
			return err
		}
	}
	if a.Word != RotateConfirmWord {
		return invalid("word", "must be %s", RotateConfirmWord)
	}
	return a.validate()
}

// String redacts the passphrases.
func (HubIdentityRotateArgs) String() string { return redactedIdentityArgs }

// GoString redacts the passphrases.
func (HubIdentityRotateArgs) GoString() string { return redactedIdentityArgs }

// HubIdentityChangePassphraseArgs are the arguments of
// hub_identity_change_passphrase.
type HubIdentityChangePassphraseArgs struct {
	// Passphrase is the current passphrase.
	Passphrase    string `json:"passphrase"`
	NewPassphrase string `json:"new_passphrase"`
	Confirm       string `json:"confirm"`
}

// Validate checks sizes and encodings.
func (a HubIdentityChangePassphraseArgs) Validate() error {
	for _, f := range []struct {
		name, v string
	}{{"passphrase", a.Passphrase}, {"new_passphrase", a.NewPassphrase}, {"confirm", a.Confirm}} {
		if err := checkPassphraseField(f.name, f.v, true); err != nil {
			return err
		}
	}
	return nil
}

// String redacts the passphrases.
func (HubIdentityChangePassphraseArgs) String() string { return redactedIdentityArgs }

// GoString redacts the passphrases.
func (HubIdentityChangePassphraseArgs) GoString() string { return redactedIdentityArgs }

// HubIdentityResult is the result of a completed identity operation. It holds
// public values only.
type HubIdentityResult struct {
	// Kind is passphrase_set, reissue, rotate or passphrase_change.
	Kind            string    `json:"kind"`
	Serial          uint64    `json:"serial"`
	NotBefore       Timestamp `json:"not_before"`
	NotAfter        Timestamp `json:"not_after"`
	RootFingerprint string    `json:"root_fingerprint"`
	OpFingerprint   string    `json:"op_fingerprint"`
	Generation      uint64    `json:"generation"`
	Warnings        []Warning `json:"warnings"`
}

// HubIdentityStatusArgs are the (empty) arguments of hub_identity_status.
type HubIdentityStatusArgs struct{}

// IdentityScopeStatus is the passphrase limit state of one scope.
type IdentityScopeStatus struct {
	Failures         int64      `json:"failures"`
	Locked           bool       `json:"locked"`
	LockedUntil      *Timestamp `json:"locked_until,omitempty"`
	RetryAfterSecs   int64      `json:"retry_after_seconds"`
	AttemptsThisHour int64      `json:"attempts_this_hour"`
}

// HubIdentityStatusResult is what the identity page shows. It comes from the
// database and needs no key file and no passphrase.
type HubIdentityStatusResult struct {
	// State is B0, B1, B2 or B3; StateLabel names it.
	State           string     `json:"state"`
	StateLabel      string     `json:"state_label"`
	PassphraseSet   bool       `json:"passphrase_set"`
	Serving         bool       `json:"serving"`
	RootFingerprint string     `json:"root_fingerprint,omitempty"`
	OpFingerprint   string     `json:"op_fingerprint,omitempty"`
	Serial          uint64     `json:"serial,omitempty"`
	NotBefore       *Timestamp `json:"not_before,omitempty"`
	NotAfter        *Timestamp `json:"not_after,omitempty"`
	// DaysLeft is the whole days until not_after (negative once expired).
	DaysLeft int `json:"days_left"`
	// Warning is "", "warning" (30 days), "error" (7 days) or "expired".
	Warning        string              `json:"warning,omitempty"`
	Expired        bool                `json:"expired"`
	MigratedFrom06 bool                `json:"migrated_from_06"`
	LastOp         string              `json:"last_op,omitempty"`
	LastOpAt       *Timestamp          `json:"last_op_at,omitempty"`
	Web            IdentityScopeStatus `json:"web"`
	CLI            IdentityScopeStatus `json:"cli"`
	Generation     uint64              `json:"generation"`
}

func init() {
	manage := MustActionSet(ActionManage)
	sessionOnly := func(name, summary string, args reflect.Type) Op {
		return Op{
			Name: name, Summary: summary, Scope: ScopeHub, Actions: manage, Mutates: true, SessionOnly: true,
			Args: args, Result: reflect.TypeFor[HubIdentityResult](),
		}
	}
	Register(Op{
		Name: OpHubIdentityStatus, Summary: "Show the state of the Hub identity: bootstrap state, root fingerprint, certificate serial and validity, expiry warning and the passphrase lockout state. It needs no passphrase.",
		Scope: ScopeHub, Actions: manage,
		Args: reflect.TypeFor[HubIdentityStatusArgs](), Result: reflect.TypeFor[HubIdentityStatusResult](),
	})
	Register(sessionOnly(OpHubIdentitySetPassphrase,
		"Set the Hub passphrase: seal the root under it and issue the first certificate (only while no sealed root exists). Browser session with a fresh sign-in only.",
		reflect.TypeFor[HubIdentitySetPassphraseArgs]()))
	Register(sessionOnly(OpHubIdentityReissue,
		"Issue a new operational key and certificate under the same root; all Worker sessions close and reconnect. Browser session with a fresh sign-in only.",
		reflect.TypeFor[HubIdentityReissueArgs]()))
	Register(sessionOnly(OpHubIdentityRotate,
		"Rotate the master key: a new root under a new passphrase, a transition signed by the old root, all invitations revoked; Workers follow the chain without re-pairing. Browser session with a fresh sign-in only.",
		reflect.TypeFor[HubIdentityRotateArgs]()))
	Register(sessionOnly(OpHubIdentityChangePassphrase,
		"Seal the same root under a new passphrase; no certificate, key or Worker session changes. Browser session with a fresh sign-in only.",
		reflect.TypeFor[HubIdentityChangePassphraseArgs]()))
}
