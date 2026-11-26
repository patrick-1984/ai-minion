package ops

import "reflect"

// OpGrantBind binds a session grant to the kernel-proven terminal
// incarnation of its source session (Verified), or unbinds it (Claimed).
const OpGrantBind = "grant_bind"

// GrantBindNote is the note of a grant_bind result that made a grant
// Verified: what a verified grant can do.
const GrantBindNote = "A verified grant is used only by that terminal, through the verified path: workers_list, sessions_list, session_snapshot and send_text as a submitted prompt to an agent pane. A Control grant gives nothing more there."

// GrantBindArgs bind (Bind true) or unbind (Bind false) one grant at the
// revision the caller looked at. Bind is required: an omitted or null bind is
// refused, so that a request that forgot it cannot unbind a Verified grant.
type GrantBindArgs struct {
	WriteArgs
	ID         string `json:"id"`
	IfRevision uint64 `json:"if_revision"`
	// Bind true binds the grant to the terminal incarnation that the source
	// Worker reports now for the source session; false makes it Claimed. A
	// pointer, so that an omitted or null bind is told apart from false.
	Bind *bool `json:"bind" openapi:"required"`
}

// Binds reports whether the request binds (true) or unbinds (false). A
// request without a bind value, which Validate refuses, unbinds nothing: it
// reports false but never reaches a handler.
func (a GrantBindArgs) Binds() bool { return a.Bind != nil && *a.Bind }

// Validate checks the request id, the grant id and the revision.
func (a GrantBindArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if a.Force {
		return invalid("force", "a grant cannot be bound by force")
	}
	if !ValidUUIDv7(a.ID) {
		return invalid("id", "must be a UUIDv7")
	}
	if a.IfRevision == 0 {
		return invalid("if_revision", "the expected revision is required")
	}
	if a.Bind == nil {
		return invalid("bind", "must be true or false")
	}
	return nil
}

func init() {
	// SessionOnly like grant_set: it returns the grant, declares no guards
	// and its handler audits it.
	Register(Op{Name: OpGrantBind, Summary: "Bind a session grant to the verified terminal of its source session, or unbind it (owner of both Workers or administrator). Browser session with a fresh sign-in only.",
		Scope: ScopeHub, Actions: MustActionSet(ActionOperate), Mutates: true, SessionOnly: true,
		Args: reflect.TypeFor[GrantBindArgs](), Result: reflect.TypeFor[GrantSetResult]()})
}
