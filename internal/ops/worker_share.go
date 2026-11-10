package ops

import (
	"reflect"
	"slices"
	"strconv"
)

// Names of the share operations (Story 4.15, spine AM-1, EPICS A-8 and A-9).
// They are Worker ops that join the catalog next to the 11 core ops
// ([CoreWorkerOpNames] stays the fixed core): the Hub routes them to the Worker
// the request names, and they need the action operate on that Worker.
const (
	OpShareGet = "share_get"
	OpShareSet = "share_set"
)

// ShareWorkerOpNames lists the two share ops, in catalog order.
var ShareWorkerOpNames = []string{OpShareGet, OpShareSet}

// The share modes a caller can set. The fourth mode of the Worker ("later":
// nothing is shared until a choice is made) is only ever reported, never set.
const (
	ShareModeAll    = "all"
	ShareModeNone   = "none"
	ShareModeSelect = "select"
	ShareModeLater  = "later"
)

// MaxShareNames bounds the session names of one share_set.
const MaxShareNames = 4096

// ShareGetArgs are the arguments of share_get.
//
// worker_id names the Worker that runs the operation and is always required:
// the Hub routes by it, and a Worker (also through its local control socket)
// acts only if it names the Worker itself. There is no implicit "this Worker":
// a name that is not the Worker's own id is refused, wherever the call comes
// from, so that a foreign id can never change the sharing of the machine the
// command happens to run on (see the Worker's requireSelf).
type ShareGetArgs struct {
	WorkerID string `json:"worker_id"`
	// WithRevision is set by the Hub, never by a caller: the answer then holds
	// the revision of the rule (see [ShareResult.Revision]).
	WithRevision bool `json:"with_revision,omitempty"`
}

// Validate checks the Worker id.
func (a ShareGetArgs) Validate() error {
	if !ValidUUIDv7(a.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	return nil
}

// ShareSetArgs are the arguments of share_set: the new share rule of the Worker.
type ShareSetArgs struct {
	// WorkerID is the Worker that runs the operation; see [ShareGetArgs].
	WorkerID string `json:"worker_id"`
	// WithRevision is set by the Hub, never by a caller; see [ShareGetArgs].
	WithRevision bool `json:"with_revision,omitempty"`
	// Mode is all (every session, also later ones), none or select (exactly the
	// sessions in Names).
	Mode string `json:"mode"`
	// Names are the sessions to share; required with select, refused otherwise.
	Names []string `json:"names,omitempty"`
	// OpSeq is set by the Hub, never by a caller: the Hub's own sequence number
	// of this change, which grows with every share_set it dispatches (also
	// across Hub restarts). The Worker applies a change only while no change
	// with an equal or higher sequence number has been seen (and raises that
	// mark to this one under the same lock that commits it): a change the Hub
	// has stopped waiting for can therefore never be applied after the Hub
	// has asked the Worker about its rule (see [ShareResult.OpBarrier]). Zero
	// (a local caller, an older Hub) is not ordered.
	OpSeq uint64 `json:"op_seq,omitempty"`
}

// Validate checks the Worker id, the mode and the names.
func (a ShareSetArgs) Validate() error {
	if !ValidUUIDv7(a.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	switch a.Mode {
	case ShareModeAll, ShareModeNone:
		if len(a.Names) > 0 {
			return invalid("names", "are only given with the mode select")
		}
	case ShareModeSelect:
		if len(a.Names) == 0 {
			return invalid("names", "select needs at least one session name; use the mode none to share nothing")
		}
		if len(a.Names) > MaxShareNames {
			return invalid("names", "at most %d session names", MaxShareNames)
		}
		for _, n := range a.Names {
			if !validSessionName(n) {
				return invalid("names", "invalid session name")
			}
		}
	default:
		return invalid("mode", "must be all, none or select")
	}
	return nil
}

// ShareRule is the share rule of a Worker as share_get and share_set report it.
type ShareRule struct {
	// Mode is all, none, select or later (nothing shared until a choice was made).
	Mode string `json:"mode"`
	// Names are the sessions selected; only with the mode select.
	Names []string `json:"names,omitempty"`
}

// ShareResult is the result of share_get and share_set: the share rule in
// force and the names of the sessions the Worker does not share. It is names
// only: no content, pane data, question or turn of any session (A-9).
type ShareResult struct {
	Rule ShareRule `json:"rule"`
	// Unshared are the names of the existing sessions that the rule does not
	// share, sorted.
	Unshared []string `json:"unshared"`
	// Locked is true when the Worker was started with a local share lock: it
	// refuses share_set.
	Locked bool `json:"locked,omitempty"`
	// Revision orders the share rules of one Worker: it is the revision of the
	// rule in force, kept durably by the Worker and raised by every change of
	// it, written as a decimal string (a 64-bit counter does not survive every
	// JSON reader as a number). The Hub applies a rule only if its revision is
	// not older than the one it holds (and only from the connection that is
	// current), so a late answer cannot undo a newer choice. It is present only
	// when the Hub asked for it (with_revision); empty means the Worker did not
	// say (an older Worker, or a caller that is not the Hub), and the Hub then
	// trusts only the unshared names. See [FormatShareRevision].
	Revision string `json:"revision,omitempty"`
	// OpBarrier is the highest [ShareSetArgs.OpSeq] the Worker has seen, as a
	// decimal string, answered with the rule: every share_set of the Hub with an
	// equal or lower sequence number is either part of the rule in this answer
	// or will never be applied. It is the proof with which the Hub settles a
	// restriction whose outcome it does not know. Empty: none seen, or an older
	// Worker (which proves nothing).
	OpBarrier string `json:"op_barrier,omitempty"`
}

// BarrierNumber is the barrier of the answer; 0 when it has none.
func (r ShareResult) BarrierNumber() uint64 {
	n, err := strconv.ParseUint(r.OpBarrier, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// ShareProbe is the answer of the Worker to the Hub's internal share probe: the
// rule in force with its revision, and the barrier (see
// [ShareResult.OpBarrier]). It holds no session content and no session list; it
// is not an operation of the catalog and no caller can ask for it: only the Hub
// sends it, on the connection of the Worker, for its own decisions.
type ShareProbe struct {
	Rule ShareRule `json:"rule"`
	// Revision is the revision of the rule in force, a decimal string.
	Revision string `json:"revision"`
	// OpBarrier is the barrier after the probe's own request (see
	// [ShareResult.OpBarrier]), a decimal string.
	OpBarrier string `json:"op_barrier"`
}

// ShareProbeRequest is the Hub's internal probe frame body. OpBarrier raises the
// Worker's barrier to at least that sequence number before it answers.
type ShareProbeRequest struct {
	Type      string `json:"type"`
	OpBarrier uint64 `json:"op_barrier,omitempty"`
}

// FormatShareRevision writes a revision as it travels in [ShareResult]; 0 (no
// revision) is the empty string.
func FormatShareRevision(n uint64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatUint(n, 10)
}

// RevisionNumber is the revision of the answer; 0 when it has none or it does
// not read as a revision.
func (r ShareResult) RevisionNumber() uint64 {
	n, err := strconv.ParseUint(r.Revision, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// ShareDecl is a share rule with its revision, as a Worker declares it on its
// event stream (in the first frame and whenever the rule changes) and as the
// Hub keeps it. It holds no session content and no session list: with the
// rule, the Hub decides for any session name, present or not, whether it is
// shared.
type ShareDecl struct {
	Rule     ShareRule `json:"rule"`
	Revision uint64    `json:"revision"`
}

// Includes reports whether the rule shares the named session: every session
// for all, none for none and later, and exactly the names for select.
func (r ShareRule) Includes(session string) bool {
	switch r.Mode {
	case ShareModeAll:
		return true
	case ShareModeSelect:
		return slices.Contains(r.Names, session)
	}
	return false
}

// Narrower reports whether r shares nothing that o does not share: every
// session r includes, o includes too.
func (r ShareRule) Narrower(o ShareRule) bool {
	switch {
	case r.Mode == ShareModeNone || r.Mode == ShareModeLater:
		return true
	case o.Mode == ShareModeAll:
		return true
	case r.Mode == ShareModeAll:
		return false
	case r.Mode == ShareModeSelect && o.Mode == ShareModeSelect:
		for _, n := range r.Names {
			if !slices.Contains(o.Names, n) {
				return false
			}
		}
		return true
	}
	return false
}

// Normalize sorts and compacts the name lists, so that a result is the same
// whatever order the Worker listed the sessions in.
func (r *ShareResult) Normalize() {
	slices.Sort(r.Unshared)
	r.Unshared = slices.Compact(r.Unshared)
	if r.Unshared == nil {
		r.Unshared = []string{}
	}
	slices.Sort(r.Rule.Names)
	r.Rule.Names = slices.Compact(r.Rule.Names)
}

func init() {
	operate := MustActionSet(ActionOperate)
	Register(Op{
		Name: OpShareGet, Summary: "Show the share rule of a Worker and the names of the sessions it does not share.",
		Scope: ScopeWorker, Actions: operate,
		Args: reflect.TypeFor[ShareGetArgs](), Result: reflect.TypeFor[ShareResult](),
	})
	// share_set changes the Worker's share rule. It is Mutates, not Write: its
	// result is the new rule, not a WriteResult, and repeating it is harmless
	// (it sets, it does not toggle), so a lost answer is simply asked again.
	Register(Op{
		Name: OpShareSet, Summary: "Set the share rule of a Worker: all, none, or select with the session names to share.",
		Scope: ScopeWorker, Actions: operate, Mutates: true,
		Args: reflect.TypeFor[ShareSetArgs](), Result: reflect.TypeFor[ShareResult](),
	})
}
