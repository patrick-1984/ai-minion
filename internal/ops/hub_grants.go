package ops

import (
	"encoding/json"
	"reflect"
	"time"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Names of the session grant operations. A grant says: source session S on
// Worker A may reach target T on Worker B at a level. The owner of the Workers
// or an administrator decides; the handler tells the two authorities apart.
const (
	OpGrantList    = "grant_list"
	OpGrantSet     = "grant_set"
	OpGrantRevoke  = "grant_revoke"
	OpGrantApprove = "grant_approve"
)

// Levels of a grant, a ladder: each level includes the ones below it.
const (
	GrantLevelView    = "view"
	GrantLevelMessage = "message"
	GrantLevelControl = "control"
)

// Values of Grant.SourceProof, Grant.Authority and Grant.State.
const (
	GrantProofClaimed  = "claimed"
	GrantProofVerified = "verified"

	GrantAuthorityOwner = "owner"
	GrantAuthorityAdmin = "admin"

	GrantStateActive    = "active"
	GrantStateExpired   = "expired"
	GrantStateSuspended = "suspended"
)

// MaxGrantNote is the longest note of a grant, in runes.
const MaxGrantNote = 200

// GrantLevelRank maps a level name to its stored rank (1 view, 2 message,
// 3 control). ok is false for any other name.
func GrantLevelRank(level string) (rank int, ok bool) {
	switch level {
	case GrantLevelView:
		return 1, true
	case GrantLevelMessage:
		return 2, true
	case GrantLevelControl:
		return 3, true
	}
	return 0, false
}

// GrantLevelName maps a stored rank back to its level name. ok is false for a
// rank outside 1 to 3.
func GrantLevelName(rank int) (level string, ok bool) {
	switch rank {
	case 1:
		return GrantLevelView, true
	case 2:
		return GrantLevelMessage, true
	case 3:
		return GrantLevelControl, true
	}
	return "", false
}

// Grant source scopes (GrantSetArgs.SourceScope, Grant.SourceScope).
const (
	// GrantSourceSession is one named source session.
	GrantSourceSession = "session"
	// GrantSourceWorker is every shared session of the source Worker,
	// sessions it shares later included.
	GrantSourceWorker = "worker"
	// GrantSourceEveryone is every shared session of every eligible Worker
	// (administrators only).
	GrantSourceEveryone = "everyone"
)

// Grant is one grant as the Hub reports it.
type Grant struct {
	ID string `json:"id"`
	// SourceScope is "session", "worker" (every shared session of the source
	// Worker: SourceSession empty) or "everyone" (no source Worker).
	SourceScope    string `json:"source_scope"`
	SourceWorkerID string `json:"source_worker_id"`
	SourceSession  string `json:"source_session"`
	// SourceProof is "claimed" (the source is identified by its name, its
	// Worker or everyone) or "verified" (a legacy grant pinned to a bound
	// master incarnation).
	SourceProof    string `json:"source_proof"`
	TargetWorkerID string `json:"target_worker_id"`
	// TargetSession is empty for every shared session of the target Worker,
	// future ones included.
	TargetSession string `json:"target_session,omitempty"`
	Level         string `json:"level"`
	// Authority is "owner" or "admin": who stands behind the grant.
	Authority    string     `json:"authority"`
	GrantedBy    string     `json:"granted_by"`
	ExpiresAt    *Timestamp `json:"expires_at,omitempty"`
	Revision     uint64     `json:"revision"`
	CreatedAt    Timestamp  `json:"created_at"`
	UpdatedAt    Timestamp  `json:"updated_at"`
	Suspended    bool       `json:"suspended"`
	SuspendCause string     `json:"suspend_cause,omitempty"`
	Note         string     `json:"note,omitempty"`
	// CommandShell is true when the owner marked the target session as a
	// command shell of the source session (A116): a send_text or send_keys of
	// the source to that session is then a normal write that the shell_pane
	// guard does not refuse, without force. Only on a Control grant that names
	// one target session.
	CommandShell bool `json:"command_shell,omitempty"`
	// FilesPull and FilesPush are the file transfer flags (files.v1): the
	// source may list and pull files from the target (needs level view or
	// more), or push files into it (needs level control).
	FilesPull bool `json:"files_pull,omitempty"`
	FilesPush bool `json:"files_push,omitempty"`
	// AppsInstall lets the source install apps on the target's Android
	// Worker from its Minion files folder (app_install, v0.25.0). Level
	// control only; never implied by files_push.
	AppsInstall bool `json:"apps_install,omitempty"`

	// Display members, filled by the handler.
	SourceWorkerName string `json:"source_worker_name,omitempty"`
	TargetWorkerName string `json:"target_worker_name,omitempty"`
	SourceOwner      string `json:"source_owner,omitempty"`
	TargetOwner      string `json:"target_owner,omitempty"`
	// State is "active", "expired" or "suspended".
	State string `json:"state,omitempty"`
	// SourceMasked and TargetMasked are true when the reader may not see the
	// session name on that side (a Worker of another owner): SourceSession or
	// TargetSession then holds the text "a session of <owner>", not a name.
	SourceMasked bool `json:"source_masked,omitempty"`
	TargetMasked bool `json:"target_masked,omitempty"`
}

// GrantListArgs are the arguments of grant_list. WorkerID limits the list to
// the grants that touch that Worker; empty lists every grant the caller may see.
type GrantListArgs struct {
	WorkerID string `json:"worker_id,omitempty"`
}

// Validate checks the optional Worker filter.
func (a GrantListArgs) Validate() error {
	if a.WorkerID != "" && !ValidUUIDv7(a.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	return nil
}

// GrantListResult lists grants.
type GrantListResult struct {
	Grants []Grant `json:"grants"`
}

// MarshalJSON writes grants as an empty array instead of null.
func (r GrantListResult) MarshalJSON() ([]byte, error) {
	type plain GrantListResult
	if r.Grants == nil {
		r.Grants = []Grant{}
	}
	return json.Marshal(plain(r))
}

// GrantTarget is one target of a grant_set that creates several grants: a
// session of a Worker, or SessionName empty for every shared session of that
// Worker.
type GrantTarget struct {
	WorkerID    string `json:"worker_id"`
	SessionName string `json:"session_name,omitempty"`
}

// GrantSetArgs create a grant (ID empty) or change the level, expiry, note
// and flags of an existing one (ID with IfRevision).
//
// The source is SourceScope: "session" (or absent: the request of before
// v0.26.0) names SourceWorkerID and SourceSession; "worker" names
// SourceWorkerID only (every shared session of it); "everyone" names neither
// (administrators only). The target is TargetWorkerID and TargetSession, or,
// to create one grant per target in one step, Targets (then TargetWorkerID
// and TargetSession are empty).
type GrantSetArgs struct {
	WriteArgs
	ID             string `json:"id,omitempty"`
	SourceScope    string `json:"source_scope,omitempty"`
	SourceWorkerID string `json:"source_worker_id,omitempty"`
	SourceSession  string `json:"source_session,omitempty"`
	TargetWorkerID string `json:"target_worker_id,omitempty"`
	// TargetSession is empty for every shared session of the target Worker.
	TargetSession string `json:"target_session,omitempty"`
	// Targets are the targets of a create of several grants: one grant per
	// distinct target, all created together or none.
	Targets []GrantTarget `json:"targets,omitempty"`
	Level   string        `json:"level"`
	// ExpiresAt is RFC 3339 UTC with milliseconds; empty means no expiry.
	ExpiresAt string `json:"expires_at,omitempty"`
	Note      string `json:"note,omitempty"`
	// IfRevision is the revision the change was decided for; required with ID.
	IfRevision uint64 `json:"if_revision,omitempty"`
	// CommandShell marks the target session as a command shell of the source
	// session (A116, see Grant.CommandShell). Absent keeps the stored flag on
	// a change (none on a create); a level below control clears it. true needs
	// level control and a target session.
	CommandShell *bool `json:"command_shell,omitempty"`
	// FilesPull and FilesPush set the file transfer flags (see
	// Grant.FilesPull). Absent keeps the stored flag on a change (off on a
	// create); files_push true needs level control.
	FilesPull *bool `json:"files_pull,omitempty"`
	FilesPush *bool `json:"files_push,omitempty"`
	// AppsInstall sets the app install flag (see Grant.AppsInstall). Absent
	// keeps the stored flag on a change (unset on a create); true needs
	// level control.
	AppsInstall *bool `json:"apps_install,omitempty"`
}

// Validate checks the shape of a grant. Whether the caller may decide it is
// the handler's question.
func (a GrantSetArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if a.Force {
		return invalid("force", "a grant cannot be forced")
	}
	if a.ID != "" {
		if !ValidUUIDv7(a.ID) {
			return invalid("id", "must be a UUIDv7")
		}
		if a.IfRevision == 0 {
			return invalid("if_revision", "the expected revision is required to change a grant")
		}
	} else if a.IfRevision != 0 {
		return invalid("if_revision", "a new grant has no revision")
	}
	switch a.SourceScope {
	case "", GrantSourceSession:
		if !ValidUUIDv7(a.SourceWorkerID) {
			return invalid("source_worker_id", "must be a UUIDv7")
		}
		if !proto.ValidMasterSessionName(a.SourceSession) {
			return invalid("source_session", "must be one valid session name")
		}
	case GrantSourceWorker:
		if !ValidUUIDv7(a.SourceWorkerID) {
			return invalid("source_worker_id", "must be a UUIDv7")
		}
		if a.SourceSession != "" {
			return invalid("source_session", "every shared session of a Worker names no session")
		}
	case GrantSourceEveryone:
		if a.SourceWorkerID != "" || a.SourceSession != "" {
			return invalid("source_worker_id", "everyone names no source Worker or session")
		}
	default:
		return invalid("source_scope", "must be session, worker or everyone")
	}
	targets, err := a.targetList()
	if err != nil {
		return err
	}
	if a.ID != "" && a.Targets != nil {
		return invalid("targets", "a change names the one target of its grant")
	}
	for _, t := range targets {
		if !ValidUUIDv7(t.WorkerID) {
			return invalid("target_worker_id", "must be a UUIDv7")
		}
		if t.WorkerID == a.SourceWorkerID {
			return invalid("target_worker_id", "source and target must be different Workers")
		}
		if t.SessionName != "" && !proto.ValidMasterSessionName(t.SessionName) {
			return invalid("target_session", "must be a valid session name, or empty for all shared sessions")
		}
		if a.CommandShell != nil && *a.CommandShell && t.SessionName == "" {
			return invalid("command_shell", "a command shell names one target session")
		}
	}
	if _, ok := GrantLevelRank(a.Level); !ok {
		return invalid("level", "must be view, message or control")
	}
	if a.ExpiresAt != "" {
		if _, err := time.Parse(timestampLayout, a.ExpiresAt); err != nil {
			return invalid("expires_at", "must be RFC 3339 UTC with milliseconds")
		}
	}
	if !utf8.ValidString(a.Note) || utf8.RuneCountInString(a.Note) > MaxGrantNote {
		return invalid("note", "must be valid text of at most %d characters", MaxGrantNote)
	}
	if a.CommandShell != nil && *a.CommandShell && a.Level != GrantLevelControl {
		return invalid("command_shell", "a command shell needs level control")
	}
	if a.FilesPush != nil && *a.FilesPush && a.Level != GrantLevelControl {
		return invalid("files_push", "pushing files needs level control")
	}
	if a.AppsInstall != nil && *a.AppsInstall && a.Level != GrantLevelControl {
		return invalid("apps_install", "installing apps needs level control")
	}
	return nil
}

// TargetList returns the targets the arguments name: Targets, or the one
// TargetWorkerID and TargetSession. Validate has checked them.
func (a GrantSetArgs) TargetList() []GrantTarget {
	t, _ := a.targetList()
	return t
}

func (a GrantSetArgs) targetList() ([]GrantTarget, error) {
	if a.Targets == nil {
		return []GrantTarget{{WorkerID: a.TargetWorkerID, SessionName: a.TargetSession}}, nil
	}
	if a.TargetWorkerID != "" || a.TargetSession != "" {
		return nil, invalid("targets", "name the targets either in targets or in target_worker_id, not both")
	}
	if len(a.Targets) == 0 {
		return nil, invalid("targets", "choose at least one target")
	}
	if len(a.Targets) > MaxGrantTargets {
		return nil, invalid("targets", "at most %d targets", MaxGrantTargets)
	}
	seen := make(map[GrantTarget]bool, len(a.Targets))
	out := make([]GrantTarget, 0, len(a.Targets))
	for _, t := range a.Targets {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, nil
}

// MaxGrantTargets bounds the targets of one grant_set.
const MaxGrantTargets = 200

// GrantSetResult is the result of grant_set and grant_approve: the outcome in
// the form of a write result, and the grant as it now stands. A grant_set
// with targets answers the first grant it created in Grant and every grant in
// Grants, and the ids of all in CreatedIDs.
type GrantSetResult struct {
	Outcome    Outcome  `json:"outcome"`
	Reason     string   `json:"reason"`
	Note       string   `json:"note,omitempty"`
	Grant      Grant    `json:"grant"`
	Grants     []Grant  `json:"grants,omitempty"`
	CreatedIDs []string `json:"created_ids,omitempty"`
}

// GrantRevokeArgs revoke one grant at the revision the caller looked at
// (D2: every write carries if_revision). A grant changed meanwhile stays.
type GrantRevokeArgs struct {
	WriteArgs
	ID         string `json:"id"`
	IfRevision uint64 `json:"if_revision"`
}

// Validate checks the request id, the grant id and the revision.
func (a GrantRevokeArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !ValidUUIDv7(a.ID) {
		return invalid("id", "must be a UUIDv7")
	}
	if a.IfRevision == 0 {
		return invalid("if_revision", "the expected revision is required")
	}
	return nil
}

// GrantApproveArgs lift the suspension of one grant at the revision the
// administrator looked at.
type GrantApproveArgs struct {
	WriteArgs
	ID         string `json:"id"`
	IfRevision uint64 `json:"if_revision"`
}

// Validate checks the request id, the grant id and the revision.
func (a GrantApproveArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if a.Force {
		return invalid("force", "an approval cannot be forced")
	}
	if !ValidUUIDv7(a.ID) {
		return invalid("id", "must be a UUIDv7")
	}
	if a.IfRevision == 0 {
		return invalid("if_revision", "the expected revision is required")
	}
	return nil
}

func init() {
	operate := MustActionSet(ActionOperate)
	Register(Op{Name: OpGrantList, Summary: "List the session grants: for an owner the grants that touch their Workers, for an administrator all of them.",
		Scope: ScopeHub, Actions: operate,
		Args: reflect.TypeFor[GrantListArgs](), Result: reflect.TypeFor[GrantListResult]()})
	// grant_set and grant_approve are SessionOnly, which the catalog allows
	// only on a Mutates op: they return the grant, not a WriteResult, declare
	// no guards, and their handlers audit them.
	Register(Op{Name: OpGrantSet, Summary: "Create session grants in one step (one source session, every shared session of a Worker, or everyone; one or several targets; level and the file and app boxes), or change the level, expiry, note or flags of an existing one (owner of the Workers or administrator). Browser session with a fresh sign-in only.",
		Scope: ScopeHub, Actions: operate, Mutates: true, SessionOnly: true,
		Args: reflect.TypeFor[GrantSetArgs](), Result: reflect.TypeFor[GrantSetResult]()})
	Register(Op{Name: OpGrantRevoke, Summary: "Revoke a session grant (owner of a Worker it touches or administrator).",
		Scope: ScopeHub, Actions: operate, Write: true, Guards: []Guard{guardWorkerKnown},
		Args: reflect.TypeFor[GrantRevokeArgs](), Result: reflect.TypeFor[WriteResult]()})
	Register(Op{Name: OpGrantApprove, Summary: "Lift the suspension of a session grant (admin only). Browser session with a fresh sign-in only.",
		Scope: ScopeHub, Actions: MustActionSet(ActionManage), Mutates: true, SessionOnly: true,
		Args: reflect.TypeFor[GrantApproveArgs](), Result: reflect.TypeFor[GrantSetResult]()})
}
