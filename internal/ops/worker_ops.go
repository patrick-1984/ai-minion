package ops

import (
	"reflect"
	"strconv"
	"unicode"
	"unicode/utf8"
)

// Names of the release-1 Worker operations (AD-2).
const (
	OpSessionsList    = "sessions_list"
	OpSessionCreate   = "session_create"
	OpSessionSnapshot = "session_snapshot"
	OpSessionCapture  = "session_capture"
	OpSendText        = "send_text"
	OpSendKeys        = "send_keys"
	OpSetTask         = "set_task"
	OpAnswer          = "answer"
	OpPipeSend        = "pipe_send"
	OpPipeRead        = "pipe_read"
	OpPipeWait        = "pipe_wait"
)

// CoreWorkerOpNames lists the 11 release-1 Worker ops declared by this file,
// in the order of the spine. Later stories add ops (for example share_get)
// from files of their own; this list stays the fixed core.
var CoreWorkerOpNames = []string{
	OpSessionsList, OpSessionCreate, OpSessionSnapshot, OpSessionCapture,
	OpSendText, OpSendKeys, OpSetTask, OpAnswer,
	OpPipeSend, OpPipeRead, OpPipeWait,
}

// Argument limits shared by every surface.
const (
	// DefaultCaptureLines is used when session_capture.lines is 0.
	DefaultCaptureLines = 200
	// MaxCaptureLines caps the scrollback lines of one capture.
	MaxCaptureLines = 10000
	// MaxKeyLen caps one send_keys key name in bytes. The longest tmux key
	// name ("C-M-S-PageDown") is far shorter; free text belongs in send_text.
	MaxKeyLen = 32
	// MaxSendKeys caps the keys of one send_keys call.
	MaxSendKeys = 256
	// MaxPipeWaitMS caps pipe_wait timeouts (10 minutes).
	MaxPipeWaitMS = 600000
	// MaxTaskLen caps the task text in bytes.
	MaxTaskLen = 1024
	// MaxPipeReadLimit caps the turns of one pipe_read.
	MaxPipeReadLimit = 200
)

// Guard declarations shared by the write ops. Each names the state it reads
// (AD-12) and its class (AD-15).
var (
	guardTargetRecheck = Guard{Name: "target_recheck", Class: GuardKeep, Reads: "pane.identity"}
	guardWriterLock    = Guard{Name: "writer_lock", Class: GuardKeep, Reads: "pane.lock"}
	guardPaneBusy      = Guard{Name: "pane_busy", Class: GuardForce, Reads: "pane.state"}
	guardPaneDialog    = Guard{Name: "pane_dialog", Class: GuardForce, Reads: "pane.state"}
	guardPaneShell     = Guard{Name: "pane_shell", Class: GuardForce, Reads: "pane.kind"}
	guardDialogBound   = Guard{Name: "dialog_binding", Class: GuardKeep, Reads: "pane.dialog"}
	guardReadiness     = Guard{Name: "readiness_unknown", Class: GuardWarn, Reads: "pane.state"}
	guardPipeLimits    = Guard{Name: "pipe_limits", Class: GuardKeep, Reads: "pipe.budget"}
	guardNameFree      = Guard{Name: "session_name_free", Class: GuardKeep, Reads: "session.name"}
	guardSessionExists = Guard{Name: "session_exists", Class: GuardKeep, Reads: "session.name"}
)

// SessionsListArgs are the arguments of sessions_list (none).
type SessionsListArgs struct{}

// SessionInfo describes one session in a listing.
type SessionInfo struct {
	Name     string    `json:"name"`
	Created  Timestamp `json:"created"`
	Windows  int       `json:"windows"`
	Attached bool      `json:"attached"`
	Task     string    `json:"task,omitempty"` // the @minion_task option
	// Probe echoes the @minion_probe option of the session, a dedicated
	// identity marker (never user text) that a client writes on its own tmux
	// server to learn whether this Worker serves that same server. Additive
	// field: a Worker that predates it never reports it. Only sessions the
	// Worker shares are listed, so it exposes nothing of an unshared session.
	Probe string    `json:"probe,omitempty"`
	Pane  *PaneInfo `json:"pane"` // the active pane, null when none
}

// SessionsListResult is the result of sessions_list. Sessions the Worker does
// not share are omitted.
type SessionsListResult struct {
	Sessions []SessionInfo `json:"sessions"`
}

// SessionCreateArgs are the arguments of session_create.
type SessionCreateArgs struct {
	WriteArgs
	// Name of the new session; empty lets the Worker choose minion-N.
	Name string `json:"name,omitempty"`
	// Cwd is the starting directory; empty means the Worker's default.
	Cwd string `json:"cwd,omitempty"`
	// Argv is a program to run directly in the pane without a shell; empty
	// starts the user's shell.
	Argv []string `json:"argv,omitempty"`
}

// Validate checks the request id, the optional name and argv.
func (a SessionCreateArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if a.Name != "" && !validSessionName(a.Name) {
		return invalid("name", "invalid session name")
	}
	if len(a.Argv) > 0 && a.Argv[0] == "" {
		return invalid("argv", "the program must not be empty")
	}
	for _, s := range a.Argv {
		if !utf8.ValidString(s) {
			return invalid("argv", "must be valid UTF-8")
		}
	}
	if !utf8.ValidString(a.Cwd) {
		return invalid("cwd", "must be valid UTF-8")
	}
	return nil
}

// SessionSnapshotArgs are the arguments of session_snapshot.
type SessionSnapshotArgs struct {
	Target Target `json:"target"`
}

// Validate checks the target.
func (a SessionSnapshotArgs) Validate() error { return a.Target.Validate() }

// SessionSnapshotResult is the visible screen of the resolved pane.
type SessionSnapshotResult struct {
	Pane   PaneInfo `json:"pane"`
	Screen Screen   `json:"screen"`
}

// SessionCaptureArgs are the arguments of session_capture.
type SessionCaptureArgs struct {
	Target Target `json:"target"`
	// Lines is the number of scrollback lines to return, counted from the
	// bottom; 0 means DefaultCaptureLines.
	Lines int `json:"lines,omitempty"`
}

// Validate checks the target and the line count.
func (a SessionCaptureArgs) Validate() error {
	if err := a.Target.Validate(); err != nil {
		return err
	}
	if a.Lines < 0 || a.Lines > MaxCaptureLines {
		return invalid("lines", "must be between 0 and %d", MaxCaptureLines)
	}
	return nil
}

// EffectiveLines returns Lines or DefaultCaptureLines when it is 0.
func (a SessionCaptureArgs) EffectiveLines() int {
	if a.Lines == 0 {
		return DefaultCaptureLines
	}
	return a.Lines
}

// SessionCaptureResult is the scrollback of the resolved pane.
type SessionCaptureResult struct {
	Pane   PaneInfo `json:"pane"`
	Screen Screen   `json:"screen"`
}

// SendTextArgs are the arguments of send_text: the text is pasted into the
// pane as typed input. The Worker adds no Enter, unless Submit is set.
//
// The text is delivered verbatim and bracketed (tmux paste-buffer -p -r). It
// may hold TAB and LF and no other control character: input controls (ESC, CR,
// NUL, DEL, C1) are refused, and intentional keys belong in send_keys. LF is
// delivered as LF. The Worker never turns it into a submission, but a program
// that does not enable bracketed paste may treat a received LF as one, so a
// multi-line text can still submit in such a program.
type SendTextArgs struct {
	WriteArgs
	Target Target `json:"target"`
	Text   string `json:"text"`
	// Submit presses Enter after the text, inside the same request: one
	// request id, one resolved pane and one pane lock for the paste and the
	// Enter. The pane identity checked before the paste is checked before the
	// Enter too. If the paste may have gone out and the Enter did not, the
	// outcome is partial or unknown, never failed_not_sent. Default false.
	Submit bool `json:"submit,omitempty"`
}

// Validate checks the request id, the target and the text.
func (a SendTextArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if err := a.Target.Validate(); err != nil {
		return err
	}
	return validPasteText("text", a.Text)
}

// SendKeysArgs are the arguments of send_keys. Keys use tmux key names such as
// "Enter", "Escape" or "C-c".
type SendKeysArgs struct {
	WriteArgs
	Target Target   `json:"target"`
	Keys   []string `json:"keys"`
}

// Validate checks the request id, the target and the key list.
func (a SendKeysArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if err := a.Target.Validate(); err != nil {
		return err
	}
	if len(a.Keys) == 0 || len(a.Keys) > MaxSendKeys {
		return invalid("keys", "must hold between 1 and %d keys", MaxSendKeys)
	}
	for _, k := range a.Keys {
		if k == "" || len(k) > MaxKeyLen || !validKeyName(k) {
			return invalid("keys", "every key must be one printable character or a tmux key name such as Enter or C-c, at most %d bytes", MaxKeyLen)
		}
	}
	return nil
}

// keyNames is the allow-list of tmux key names (without modifiers) that
// send_keys accepts. Free text is never a key: it belongs in send_text.
var keyNames = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range []string{
		"Enter", "Escape", "Tab", "BTab", "BSpace", "Space",
		"Up", "Down", "Left", "Right", "Home", "End",
		"PageUp", "PPage", "PgUp", "PageDown", "NPage", "PgDn",
		"IC", "DC",
		"KP0", "KP1", "KP2", "KP3", "KP4", "KP5", "KP6", "KP7", "KP8", "KP9",
		"KP/", "KP*", "KP-", "KP+", "KP.", "KPEnter",
	} {
		m[n] = true
	}
	for i := 1; i <= 12; i++ {
		m["F"+strconv.Itoa(i)] = true
	}
	return m
}()

// validKeyName reports whether k is a key name and not free text: a single
// printable rune, or a tmux key name from the allow-list, optionally prefixed
// by any combination of the modifiers C-, M- and S-. Raw escape bytes, line
// breaks and words never reach the tmux argv (AD-13).
func validKeyName(k string) bool {
	if !utf8.ValidString(k) {
		return false
	}
	for {
		if r, n := utf8.DecodeRuneInString(k); n > 0 && n == len(k) {
			return r != utf8.RuneError && unicode.IsPrint(r)
		}
		if keyNames[k] {
			return true
		}
		if len(k) > 2 && k[1] == '-' && (k[0] == 'C' || k[0] == 'M' || k[0] == 'S') {
			k = k[2:]
			continue
		}
		return false
	}
}

// SetTaskArgs are the arguments of set_task. An empty Task clears the task.
type SetTaskArgs struct {
	WriteArgs
	Target Target `json:"target"`
	Task   string `json:"task"`
}

// Validate checks the request id, the target and the task text.
func (a SetTaskArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if err := a.Target.Validate(); err != nil {
		return err
	}
	if len(a.Task) > MaxTaskLen || !utf8.ValidString(a.Task) {
		return invalid("task", "must be valid UTF-8 of at most %d bytes", MaxTaskLen)
	}
	return nil
}

// AnswerArgs are the arguments of answer: choose one option of the dialog the
// pane shows. The Worker sends the keys of that option, but only to the dialog
// the caller reviewed.
type AnswerArgs struct {
	WriteArgs
	Target Target `json:"target"`
	// Choice names the option of the reviewed dialog. A decimal number is the
	// 1-based position of the option and always means the position, even when
	// an option has a numeric label; anything else is an option label or key,
	// for example "yes" or "no".
	Choice string `json:"choice"`
	// Expect is the pane and the dialog the caller reviewed, as a snapshot or
	// capture reported them (PaneInfo). The Worker compares it with the pane
	// under the pane lock immediately before it sends the keys, and refuses
	// with pane_changed or dialog_changed when it differs. This is KEEP: force
	// never overrides it, and the Worker never replaces a stale Expect with
	// the current dialog.
	Expect ReviewedDialog `json:"expect"`
}

// Validate checks the request id, the target, the choice and the binding.
func (a AnswerArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if err := a.Target.Validate(); err != nil {
		return err
	}
	if a.Choice == "" || len(a.Choice) > 64 || !utf8.ValidString(a.Choice) {
		return invalid("choice", "must be a non-empty UTF-8 string of at most 64 bytes")
	}
	return a.Expect.Validate()
}

// DialogDigestLen is the length of a dialog digest: lowercase hex of SHA-256.
const DialogDigestLen = 64

// ReviewedDialog identifies the dialog a caller looked at. Every field is
// required and is copied from the PaneInfo of the snapshot or capture the
// caller reviewed. The session name stays the identity of the target (R88);
// this adds the write precondition for the process and the dialog behind it.
type ReviewedDialog struct {
	// PaneID is PaneInfo.ID. A recreated session has new pane ids.
	PaneID string `json:"pane_id"`
	// PID is PaneInfo.PID, the process id of the pane's program.
	PID int `json:"pid"`
	// Command is PaneInfo.Command, the foreground command.
	Command string `json:"command"`
	// SessionCreated is PaneInfo.SessionCreated, the generation of the
	// session.
	SessionCreated int64 `json:"session_created"`
	// DialogDigest is PaneInfo.DialogDigest, the semantic digest of the
	// question and its options.
	DialogDigest string `json:"dialog_digest"`
}

// Limits of the binding fields.
const (
	maxPaneIDLen  = 32
	maxCommandLen = 256
)

// Validate checks that every field is present and well-formed.
func (r ReviewedDialog) Validate() error {
	if r.PaneID == "" || len(r.PaneID) > maxPaneIDLen || !validToken(r.PaneID) {
		return invalid("expect.pane_id", "is required: the pane id of the reviewed dialog")
	}
	if r.PID <= 0 {
		return invalid("expect.pid", "is required: the process id of the reviewed pane")
	}
	if r.Command == "" || len(r.Command) > maxCommandLen || !utf8.ValidString(r.Command) || hasControl(r.Command) {
		return invalid("expect.command", "is required: the command of the reviewed pane")
	}
	if r.SessionCreated <= 0 {
		return invalid("expect.session_created", "is required: the session generation of the reviewed pane")
	}
	if !ValidDialogDigest(r.DialogDigest) {
		return invalid("expect.dialog_digest", "is required: the digest of the reviewed dialog (%d lowercase hex characters)", DialogDigestLen)
	}
	return nil
}

// ValidDialogDigest reports whether s has the form of a dialog digest.
func ValidDialogDigest(s string) bool {
	if len(s) != DialogDigestLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && (s[i] < 'a' || s[i] > 'f') {
			return false
		}
	}
	return true
}

func validToken(s string) bool {
	for _, r := range s {
		if r <= ' ' || r >= 0x7f {
			return false
		}
	}
	return true
}

func hasControl(s string) bool {
	for _, r := range s {
		if isControl(r) {
			return true
		}
	}
	return false
}

// PipeSendArgs are the arguments of pipe_send. The size limit of 8 KiB and the
// rate limits belong to the broker (AD-23), not to this declaration.
type PipeSendArgs struct {
	WriteArgs
	Target Target `json:"target"`
	Text   string `json:"text"`
	// MessageID is the id the Hub gave the message it stores for this send. The
	// Hub sets it, on the op it routes to the Worker, and overwrites whatever a
	// caller put there; the Worker names it on the turn that answers the
	// delivery, which is how the Hub knows the turn is a reply to something it
	// sent (internal/hub/workers/pipe.go). Zero outside a Hub send.
	MessageID int64 `json:"message_id,omitempty"`
}

// Validate checks the request id, the target and the text.
func (a PipeSendArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if err := a.Target.Validate(); err != nil {
		return err
	}
	return validText("text", a.Text)
}

// PipeReadArgs are the arguments of pipe_read.
type PipeReadArgs struct {
	Target Target `json:"target"`
	// AfterTurnID returns only turns after this one; empty starts at the
	// oldest retained turn.
	AfterTurnID string `json:"after_turn_id,omitempty"`
	// Limit caps the turns returned; 0 means the broker default.
	Limit int `json:"limit,omitempty"`
}

// Validate checks the target, the cursor and the limit.
func (a PipeReadArgs) Validate() error {
	if err := a.Target.Validate(); err != nil {
		return err
	}
	if a.AfterTurnID != "" && !ValidUUIDv7(a.AfterTurnID) {
		return invalid("after_turn_id", "must be a UUIDv7")
	}
	if a.Limit < 0 || a.Limit > MaxPipeReadLimit {
		return invalid("limit", "must be between 0 and %d", MaxPipeReadLimit)
	}
	return nil
}

// Pipe turn directions.
const (
	// PipeSent is a message a peer sent to the agent.
	PipeSent = "sent"
	// PipeReceived is a turn extracted from the agent's reply.
	PipeReceived = "received"
)

// PipeTurn is one entry of the pipe of a session.
type PipeTurn struct {
	TurnID    string    `json:"turn_id"`   // UUIDv7
	Session   string    `json:"session"`   // session name
	Direction string    `json:"direction"` // PipeSent or PipeReceived
	Text      string    `json:"text"`      // plain text, sanitised
	At        Timestamp `json:"at"`
}

// PipeReadResult is the result of pipe_read. A Hub that withholds the received
// turns of a source the receiving user has not trusted (AD-17, AD-22, R29)
// says so with Withheld, Reason and Notice; a Worker in Solo mode sets none of
// them. A client passes the three on to the model unchanged (never the turns
// themselves) and may add its own withholding.
type PipeReadResult struct {
	Turns []PipeTurn `json:"turns"`
	// Withheld counts the received turns that were not returned because the
	// source is not trusted by the receiving user.
	Withheld int `json:"withheld,omitempty"`
	// Reason is "untrusted_peer" when turns were withheld.
	Reason string `json:"reason,omitempty"`
	// Notice explains the withholding in English; it never carries the text of
	// a turn.
	Notice string `json:"notice,omitempty"`
}

// PipeWaitArgs are the arguments of pipe_wait.
type PipeWaitArgs struct {
	Target Target `json:"target"`
	// AfterTurnID starts after this turn; empty means only new turns.
	AfterTurnID string `json:"after_turn_id,omitempty"`
	// TimeoutMS ends the stream when no turn arrived; 0 means the broker
	// default.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// Validate checks the target, the cursor and the timeout.
func (a PipeWaitArgs) Validate() error {
	if err := a.Target.Validate(); err != nil {
		return err
	}
	if a.AfterTurnID != "" && !ValidUUIDv7(a.AfterTurnID) {
		return invalid("after_turn_id", "must be a UUIDv7")
	}
	if a.TimeoutMS < 0 || a.TimeoutMS > MaxPipeWaitMS {
		return invalid("timeout_ms", "must be between 0 and %d", MaxPipeWaitMS)
	}
	return nil
}

// validText accepts any valid UTF-8 text. Control characters are legal input
// here (newlines, tabs); size limits are enforced by the layer that owns them.
func validText(field, s string) error {
	if s == "" {
		return invalid(field, "must not be empty")
	}
	if !utf8.ValidString(s) {
		return invalid(field, "must be valid UTF-8")
	}
	return nil
}

// CheckPasteText validates the text of a paste the way every terminal adapter
// must: non-empty valid UTF-8 without any control character except TAB and LF.
// The error matches ErrInvalidArgument. It is the one rule of what a paste may
// carry, shared by the tmux and ConPTY adapters and the Hub (SendTextArgs).
func CheckPasteText(text string) error { return validPasteText("text", text) }

// PlainText reports whether text holds no control character at all, TAB and LF
// included: a single line of printable text. Every Worker, old or new, delivers
// such a text the same way, so the Hub may hand it to a Worker that does not
// announce the verbatim paste; anything else needs that feature.
func PlainText(text string) bool {
	for _, r := range text {
		if isControl(r) {
			return false
		}
	}
	return true
}

// validPasteText accepts the text of a paste: valid UTF-8 without any control
// character except TAB and LF. ESC, CR, NUL, DEL and the C1 range are input
// controls: a paste would deliver them as keystrokes (a CR submits, an ESC can
// end a bracketed paste early). Intentional control input is a send_keys call.
func validPasteText(field, s string) error {
	if err := validText(field, s); err != nil {
		return err
	}
	for _, r := range s {
		if r != '\t' && r != '\n' && isControl(r) {
			return invalid(field, "must not contain control characters other than TAB and LF; send keys with send_keys")
		}
	}
	return nil
}

func init() {
	// The common guard list of a write that lands in a pane.
	paneGuards := func(extra ...Guard) []Guard {
		return append([]Guard{guardTargetRecheck, guardWriterLock}, extra...)
	}
	read := MustActionSet(ActionRead)
	operate := MustActionSet(ActionOperate)

	Register(Op{
		Name: OpSessionsList, Summary: "List the sessions the Worker shares.",
		Scope: ScopeWorker, Actions: read,
		Args: reflect.TypeFor[SessionsListArgs](), Result: reflect.TypeFor[SessionsListResult](),
	})
	Register(Op{
		Name: OpSessionCreate, Summary: "Create a detached session, named minion-N unless a name is given.",
		Scope: ScopeWorker, Actions: operate, Write: true,
		Guards: []Guard{guardNameFree},
		Args:   reflect.TypeFor[SessionCreateArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name: OpSessionSnapshot, Summary: "Return the visible screen of a session pane.",
		Scope: ScopeWorker, Actions: read,
		Args: reflect.TypeFor[SessionSnapshotArgs](), Result: reflect.TypeFor[SessionSnapshotResult](),
	})
	Register(Op{
		Name: OpSessionCapture, Summary: "Return scrollback lines of a session pane.",
		Scope: ScopeWorker, Actions: read,
		Args: reflect.TypeFor[SessionCaptureArgs](), Result: reflect.TypeFor[SessionCaptureResult](),
	})
	Register(Op{
		Name: OpSendText, Summary: "Type text into a session pane. No Enter is added unless submit is true.",
		Scope: ScopeWorker, Actions: operate, Write: true,
		Guards: paneGuards(guardPaneBusy, guardPaneDialog, guardPaneShell, guardReadiness),
		Args:   reflect.TypeFor[SendTextArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name: OpSendKeys, Summary: "Send named keys to a session pane.",
		Scope: ScopeWorker, Actions: operate, Write: true,
		Guards: paneGuards(guardPaneBusy, guardPaneDialog, guardPaneShell, guardReadiness),
		Args:   reflect.TypeFor[SendKeysArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name: OpSetTask, Summary: "Set the task text of a session.",
		Scope: ScopeWorker, Actions: operate, Write: true,
		Guards: []Guard{guardSessionExists},
		Args:   reflect.TypeFor[SetTaskArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name: OpAnswer, Summary: "Answer the dialog a session pane shows.",
		Scope: ScopeWorker, Actions: operate, Write: true,
		Guards: paneGuards(guardDialogBound),
		Args:   reflect.TypeFor[AnswerArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name: OpPipeSend, Summary: "Send a pipe message to an agent session: it is pasted and submitted with Enter.",
		Scope: ScopeWorker, Actions: MustActionSet(ActionMessage, ActionOperate), Write: true,
		Guards: paneGuards(guardPipeLimits, guardPaneBusy, guardPaneDialog, guardPaneShell, guardReadiness),
		Args:   reflect.TypeFor[PipeSendArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name: OpPipeRead, Summary: "Read the stored pipe turns of a session.",
		Scope: ScopeWorker, Actions: read,
		Args: reflect.TypeFor[PipeReadArgs](), Result: reflect.TypeFor[PipeReadResult](),
	})
	Register(Op{
		Name: OpPipeWait, Summary: "Stream the next pipe turns of a session as they arrive.",
		Scope: ScopeWorker, Actions: read, Stream: true,
		Args: reflect.TypeFor[PipeWaitArgs](), Result: reflect.TypeFor[PipeTurn](),
	})
}

// hiddenMembers lists the argument members that belong to the Hub's own use of
// an op and that no producer (the CLI, MCP) may offer or set: pipe_send's
// message_id names the delivery the Hub stored, and the Hub overwrites it on
// the request it routes. Showing it as a flag or a tool argument would suggest
// a caller can pick it.
var hiddenMembers = map[string]map[string]bool{
	OpPipeSend: {"message_id": true},
	// with_revision is set by the Hub on the share ops it routes: the Worker
	// then adds the revision of its share rule to the answer. A caller that does
	// not come through the Hub gets the answer as it always was.
	OpShareGet: {"with_revision": true},
	OpShareSet: {"with_revision": true},
}

// memberDocs are the descriptions of argument members that differ by operation.
// The same member name means different things in different ops (send_text.text
// is typed and waits for an Enter that only submit adds, pipe_send.text is a
// message that is always submitted), so a producer must not describe it once
// for all ops. This is the one place of those descriptions: the MCP schema and
// the CLI help both read it.
var memberDocs = map[string]map[string]string{
	OpSendText: {
		"text": "The text to type, pasted verbatim. Tab and line feed are allowed; other control characters are refused (use send_keys for keys). No Enter is added unless submit is true.",
	},
	OpPipeSend: {
		"text": "The message to send, pasted verbatim into the agent's input. Tab and line feed are allowed; other control characters are refused. pipe_send always presses Enter after the message, in the same request: there is no submit argument, and the agent receives the message as a prompt.",
	},
}

// MemberDoc returns the operation-specific description of an argument member
// of op, and "" when the member has none of its own.
func MemberDoc(op, member string) string { return memberDocs[op][member] }

// HiddenMember reports whether the argument member of op is internal: producers
// derive no flag or schema property for it and refuse it as an argument.
func HiddenMember(op, member string) bool { return hiddenMembers[op][member] }
