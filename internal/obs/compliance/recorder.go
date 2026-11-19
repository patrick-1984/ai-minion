package compliance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Limits of one record (design §5.5, §5.8).
const (
	// MaxBody caps the content of one record; a longer one is cut on a rune
	// boundary and says how much was cut (minion.content.truncated_bytes).
	MaxBody = 64 << 10
	// MaxAttrBytes caps the attributes of one record.
	MaxAttrBytes = 8 << 10
	// maxAttrString caps one string attribute.
	maxAttrString = 1024
	maxTurnIDs    = 256
)

// Client names of the origin of a call.
const (
	ClientBrowser    = "browser"
	ClientAPI        = "api"
	ClientCallStream = "call-stream"
	ClientGrant      = "grant"
)

// CallInfo describes a call that reached a front door and was authorised or
// is about to be refused. The front doors fill it from what they already
// resolved; nothing here comes from the caller's own JSON.
type CallInfo struct {
	Path Path
	Op   string
	// Args is the pointer to the typed arguments (the op's Args type).
	Args any
	// Client is one of the Client constants.
	Client string
	// PrincipalKind is the authz kind (browser_session, api_key, act_as,
	// worker); Actor and ActingAs the audit identities, KeyID the API key id.
	PrincipalKind, Actor, ActingAs, KeyID string
	// WorkerID is the Worker the call is addressed to when the arguments do
	// not name it (sessions_list, session_create).
	WorkerID string
}

// Capture is the recording of one call. It is nil when nothing is recorded,
// and every method accepts a nil receiver, so the front doors call it
// unconditionally. It is used by the one goroutine of the call.
type Capture struct {
	m      *Monitor
	ctx    context.Context
	info   CallInfo
	pol    Policy
	row    Row
	worker string
	sess   string
	pane   string
	reqID  string

	contentKnown, contentOK bool
	elements                int
	denied                  bool
}

// Begin starts the recording of a call, or returns nil when the policy
// records nothing for it (off, or a path the op is not captured on).
func (m *Monitor) Begin(ctx context.Context, info CallInfo) *Capture {
	if m == nil {
		return nil
	}
	pol := m.Policy()
	if !pol.Records() {
		return nil
	}
	row := RowOf(info.Op)
	if row.Paths&info.Path == 0 {
		return nil
	}
	c := &Capture{m: m, ctx: ctx, info: info, pol: pol, row: row, worker: info.WorkerID}
	if t, ok := targetOf(info.Args); ok {
		if t.WorkerID != "" {
			c.worker = t.WorkerID
		}
		c.sess, c.pane = t.SessionName, t.Pane
	}
	c.reqID = requestIDOf(info.Args)
	return c
}

// Denied records a call that authorisation refused: an action record with the
// outcome denied and no content.
func (c *Capture) Denied() {
	if c == nil {
		return
	}
	c.denied = true
	c.m.emit(c.action("denied", nil))
}

// Attempted records, before the call is routed, the content the Hub is about
// to send (minion.session.input, status attempted). It writes nothing when
// input is not recorded, the op carries none, or the Worker is not
// user-managed.
func (c *Capture) Attempted() {
	if c == nil || c.denied || !c.pol.Input || c.row.Input == InNone || !c.content() {
		return
	}
	body, kind := inputOf(c.row.Input, c.info.Args)
	attrs := c.base()
	attrs = append(attrs, Attr{"minion.input.status", "attempted"}, Attr{"minion.input.kind", kind})
	c.m.emit(c.withBody(EventInput, attrs, body))
}

// Done records how the call ended: the action record with its outcome, then
// the output content of a read. code is the error code of a refused call, ""
// for a result.
func (c *Capture) Done(result any, code string) {
	if c == nil || c.denied {
		return
	}
	res := deref(result)
	outcome, extra := c.outcome(res, code)
	extra = append(extra, c.describe(res, code)...)
	c.m.emit(c.action(outcome, extra))
	if code != "" || !c.pol.captureOutput() || c.row.Output == OutNone || !c.content() {
		return
	}
	switch r := res.(type) {
	case ops.SessionSnapshotResult:
		c.screenOutput("snapshot", r.Pane.ID, r.Screen)
	case ops.SessionCaptureResult:
		c.screenOutput("capture", r.Pane.ID, r.Screen)
	case ops.PipeReadResult:
		for _, t := range r.Turns {
			c.turnOutput(t)
		}
	}
}

// Element records one element of a streaming read (pipe_wait): an action
// record naming the turn and, at the output level, its text once per turn id.
func (c *Capture) Element(v any) {
	if c == nil || c.denied {
		return
	}
	t, ok := deref(v).(ops.PipeTurn)
	if !ok {
		return
	}
	c.elements++
	attrs := []Attr{{"minion.outcome", "ok"}, {"minion.turn.id", t.TurnID}}
	c.m.emit(c.recordWith(EventAction, attrs, ""))
	if c.pol.captureOutput() && c.row.Output == OutTurns && c.content() {
		c.turnOutput(t)
	}
}

// End closes a streaming read: one action record with the number of elements
// and the outcome.
func (c *Capture) End(code string) {
	if c == nil || c.denied {
		return
	}
	outcome := "ok"
	if code != "" {
		outcome = "refused:" + code
	}
	c.m.emit(c.recordWith(EventAction, []Attr{{"minion.outcome", outcome}, {"minion.stream.elements", int64(c.elements)}}, ""))
}

// content reports whether content may be recorded for this call: the Worker
// is known and user-managed. It is asked once, and an error counts as no.
func (c *Capture) content() bool {
	if c.contentKnown {
		return c.contentOK
	}
	c.contentKnown = true
	deps := c.m.deps.Load()
	if c.worker == "" || deps.UserManaged == nil {
		return false
	}
	ok, err := deps.UserManaged(c.ctx, c.worker)
	c.contentOK = err == nil && ok
	return c.contentOK
}

// base are the attributes every record of the call carries.
func (c *Capture) base() []Attr {
	a := []Attr{
		{"minion.policy.version", c.pol.Version},
		{"minion.op", c.info.Op},
		{"minion.path", pathName(c.info.Path)},
		{"minion.client", c.info.Client},
		{"minion.principal.kind", c.info.PrincipalKind},
		{"minion.actor", c.info.Actor},
	}
	if c.info.ActingAs != "" {
		a = append(a, Attr{"minion.acting_as", c.info.ActingAs})
	}
	if c.info.KeyID != "" {
		a = append(a, Attr{"minion.api_key.id", c.info.KeyID})
	}
	if c.worker != "" {
		a = append(a, Attr{"minion.worker.id", c.worker})
	}
	if c.sess != "" {
		a = append(a, Attr{"minion.session", c.sess})
	}
	if c.pane != "" {
		a = append(a, Attr{"minion.pane", c.pane})
	}
	if c.reqID != "" {
		a = append(a, Attr{"minion.request.id", c.reqID})
	}
	return a
}

func pathName(p Path) string {
	if p == PathCall {
		return "call"
	}
	return "rest"
}

func (c *Capture) recordWith(event string, extra []Attr, body string) Record {
	attrs := append(c.base(), extra...)
	return c.withBody(event, attrs, body)
}

func (c *Capture) withBody(event string, attrs []Attr, body string) Record {
	r := Record{Event: event, Time: c.m.now(), Attrs: attrs}
	if body != "" {
		// Minion's own secret formats are masked before the cut, so a token
		// that straddles the limit is not left half readable.
		body = logx.RedactTokens(body)
		if len(body) > MaxBody {
			cut := MaxBody
			for cut > 0 && !utf8.RuneStart(body[cut]) {
				cut--
			}
			r.Attrs = append(r.Attrs, Attr{"minion.content.truncated_bytes", int64(len(body) - cut)})
			body = body[:cut]
		}
		r.Body = body
	}
	r.Attrs = capAttrs(r.Attrs)
	return r
}

func (c *Capture) action(outcome string, extra []Attr) Record {
	return c.recordWith(EventAction, append([]Attr{{"minion.outcome", outcome}}, extra...), "")
}

// outcome derives the outcome of the call and the replayed flag.
func (c *Capture) outcome(res any, code string) (string, []Attr) {
	if code != "" {
		return "refused:" + code, nil
	}
	if wr, ok := res.(ops.WriteResult); ok {
		var a []Attr
		if wr.Reason != "" {
			a = append(a, Attr{"minion.reason", wr.Reason})
		}
		replayed := "unknown"
		if d := c.m.deps.Load(); d.WorkerHasFeature != nil && c.worker != "" && d.WorkerHasFeature(c.worker, proto.FeatureArgsDigest) {
			replayed = strconv.FormatBool(wr.Replayed)
		}
		a = append(a, Attr{"minion.replayed", replayed})
		if wr.Pane != nil {
			a = append(a, Attr{"minion.pane.id", wr.Pane.ID})
		}
		return string(wr.Outcome), a
	}
	return "ok", nil
}

// describe adds the per-op fields of the action record (design §5.3).
func (c *Capture) describe(res any, code string) []Attr {
	var a []Attr
	args := deref(c.info.Args)
	switch c.info.Op {
	case ops.OpSessionsList:
		if r, ok := res.(ops.SessionsListResult); ok {
			a = append(a, Attr{"minion.sessions.count", int64(len(r.Sessions))})
		}
	case ops.OpSessionSnapshot:
		if r, ok := res.(ops.SessionSnapshotResult); ok {
			a = append(a, Attr{"minion.pane.id", r.Pane.ID}, Attr{"minion.screen.rows", int64(r.Screen.Height)},
				Attr{"minion.screen.cols", int64(r.Screen.Width)}, Attr{"minion.content.sha256", fingerprint(r.Screen)})
		}
	case ops.OpSessionCapture:
		if r, ok := res.(ops.SessionCaptureResult); ok {
			a = append(a, Attr{"minion.pane.id", r.Pane.ID}, Attr{"minion.lines.returned", int64(len(r.Screen.Lines))},
				Attr{"minion.content.sha256", fingerprint(r.Screen)})
		}
		if q, ok := args.(ops.SessionCaptureArgs); ok {
			a = append(a, Attr{"minion.lines.requested", int64(q.EffectiveLines())})
		}
	case ops.OpSessionCreate:
		if q, ok := args.(ops.SessionCreateArgs); ok && q.Name != "" {
			a = append(a, Attr{"minion.session.name", q.Name})
		} else if wr, ok := res.(ops.WriteResult); ok && wr.Pane != nil {
			a = append(a, Attr{"minion.session.name", wr.Pane.Session})
		}
	case ops.OpSendText:
		if q, ok := args.(ops.SendTextArgs); ok {
			a = append(a, Attr{"minion.input.length", int64(len(q.Text))}, Attr{"minion.input.submit", q.Submit})
		}
	case ops.OpSendKeys:
		if q, ok := args.(ops.SendKeysArgs); ok {
			a = append(a, Attr{"minion.input.keys", int64(len(q.Keys))})
		}
	case ops.OpSetTask:
		if q, ok := args.(ops.SetTaskArgs); ok {
			a = append(a, Attr{"minion.input.length", int64(len(q.Task))})
		}
	case ops.OpPipeSend:
		if q, ok := args.(ops.PipeSendArgs); ok {
			a = append(a, Attr{"minion.input.length", int64(len(q.Text))})
		}
	case ops.OpPipeRead:
		if r, ok := res.(ops.PipeReadResult); ok {
			ids := make([]string, 0, len(r.Turns))
			for _, t := range r.Turns {
				if len(ids) < maxTurnIDs {
					ids = append(ids, t.TurnID)
				}
			}
			a = append(a, Attr{"minion.turn.ids", ids}, Attr{"minion.pipe.withheld", int64(r.Withheld)})
		}
	}
	return a
}

// screenOutput emits the output record of a snapshot or capture: the text when
// its fingerprint is new for the scope, only the fingerprint otherwise.
func (c *Capture) screenOutput(kind, paneID string, s ops.Screen) {
	fp := fingerprint(s)
	scope := c.worker + "\x00" + c.sess + "\x00" + paneID + "\x00" + kind
	changed := c.m.dedupe.ScreenChanged(scope, fp)
	attrs := []Attr{{"minion.output.kind", kind}, {"minion.content.sha256", fp}, {"minion.content.lines", int64(len(s.Lines))}}
	body := ""
	if changed {
		body = strings.Join(s.Lines, "\n")
		attrs = append(attrs, Attr{"minion.content.repeated", false})
	} else {
		attrs = append(attrs, Attr{"minion.content.repeated", true})
	}
	if paneID != "" {
		attrs = append(attrs, Attr{"minion.pane.id", paneID})
	}
	c.m.emit(c.recordWith(EventOutput, attrs, body))
}

// turnOutput emits the text of a pipe turn the first time it is returned.
func (c *Capture) turnOutput(t ops.PipeTurn) {
	if !c.m.dedupe.TurnNew(c.worker + "\x00" + t.Session + "\x00" + t.TurnID) {
		return
	}
	attrs := []Attr{{"minion.output.kind", "turn"}, {"minion.turn.id", t.TurnID}, {"minion.turn.direction", t.Direction},
		{"minion.content.sha256", sum256(t.Text)}}
	c.m.emit(c.recordWith(EventOutput, attrs, t.Text))
}

// fingerprint is the SHA-256 of the screen payload that was returned: its
// size and its lines (style spans and cursor excluded).
func fingerprint(s ops.Screen) string {
	h := sha256.New()
	_, _ = h.Write([]byte(strconv.Itoa(s.Width) + "x" + strconv.Itoa(s.Height) + "\n"))
	for _, l := range s.Lines {
		_, _ = h.Write([]byte(l))
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sum256(s string) string {
	b := sha256.Sum256([]byte(s))
	return hex.EncodeToString(b[:])
}

// inputOf returns the content and the kind of the input of a write.
func inputOf(k InputKind, args any) (string, string) {
	switch a := deref(args).(type) {
	case ops.SendTextArgs:
		return a.Text, "text"
	case ops.SendKeysArgs:
		return strings.Join(a.Keys, " "), "keys"
	case ops.AnswerArgs:
		return a.Choice, "choice"
	case ops.SetTaskArgs:
		return a.Task, "task"
	case ops.PipeSendArgs:
		return a.Text, "message"
	case ops.SessionCreateArgs:
		b, _ := json.Marshal(struct {
			Cwd  string   `json:"cwd,omitempty"`
			Argv []string `json:"argv,omitempty"`
		}{a.Cwd, a.Argv})
		return string(b), "create"
	}
	return "", ""
}

// targetOf returns the Target member of typed arguments.
func targetOf(args any) (ops.Target, bool) {
	v := reflect.ValueOf(args)
	for v.IsValid() && v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ops.Target{}, false
		}
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return ops.Target{}, false
	}
	f := v.FieldByName("Target")
	if !f.IsValid() {
		return ops.Target{}, false
	}
	t, ok := f.Interface().(ops.Target)
	return t, ok
}

// requestIDOf returns the request id of write arguments.
func requestIDOf(args any) string {
	v := reflect.ValueOf(args)
	for v.IsValid() && v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return ""
	}
	if f := v.FieldByName("WriteArgs"); f.IsValid() {
		if w, ok := f.Interface().(ops.WriteArgs); ok {
			return w.RequestID
		}
	}
	return ""
}

// deref returns the value a pointer points to (a value stays as it is).
func deref(v any) any {
	rv := reflect.ValueOf(v)
	for rv.IsValid() && rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return nil
	}
	return rv.Interface()
}

// capAttrs bounds the attributes of a record: one string to maxAttrString
// bytes (on a rune boundary), the whole set to MaxAttrBytes (later attributes
// are dropped, never earlier ones: the identity comes first).
func capAttrs(in []Attr) []Attr {
	out := in[:0:0]
	total := 0
	for _, a := range in {
		if s, ok := a.Value.(string); ok && len(s) > maxAttrString {
			cut := maxAttrString
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
			a.Value = s[:cut]
		}
		n := len(a.Key) + attrSize(a.Value)
		if total+n > MaxAttrBytes {
			continue
		}
		total += n
		out = append(out, a)
	}
	return out
}

func attrSize(v any) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case []string:
		n := 0
		for _, s := range x {
			n += len(s) + 1
		}
		return n
	}
	return 8
}
