package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// fakeClock is a store.Clock the test moves.
type fakeClock struct{ ms atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.ms.Store(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC).UnixMilli())
	return c
}
func (c *fakeClock) Now() time.Time          { return time.UnixMilli(c.ms.Load()).UTC() }
func (c *fakeClock) Advance(d time.Duration) { c.ms.Add(d.Milliseconds()) }

type pasteCall struct{ Pane, Text string }

// fakeHost is a term.SessionHost over an in-memory list of sessions.
type fakeHost struct {
	mu       sync.Mutex
	sessions []term.Session
	screens  map[string]ops.Screen // by pane id
	options  map[string]string     // "session|key" -> value
	nextPane int

	pasted    []pasteCall
	keysSent  [][]string
	snapshots int
	// events is the order of every delivery: "paste <pane>" and
	// "keys <pane> <key,key>". Tests of a paste followed by Enter read it.
	events []string

	pasteErr  error
	keysErr   error
	keysAfter int // keys that go out before keysErr; -1 means none of them
	listErr   error

	// onSnapshot runs at the start of Snapshot, outside the lock.
	onSnapshot func()
	// onPaste runs inside PasteText, before it returns; tests block here.
	onPaste func()
	// resolveHook may replace the answer of ResolvePane.
	resolveHook func(call int, t ops.Target) (term.PaneRef, bool)
	resolveCall int
	// createErr is returned by CreateSession (after the optional side effect
	// of createRace).
	createErr  error
	createRace string // a session that appears when CreateSession is called
	// createAfter makes CreateSession create the session and then fail with
	// createErr, like an adapter whose readback failed.
	createAfter bool
	// createGone makes CreateSession start the program, which exits at once,
	// so that no session is left, and then fail with createErr.
	createGone bool
	// creates counts the programs CreateSession started.
	creates    int
	createCwd  string
	createArgv []string
	// closes counts the Close calls (the host owns resources, like ConPTY).
	closes int
}

// Close makes the fake host an io.Closer.
func (h *fakeHost) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closes++
	return nil
}

func newFakeHost() *fakeHost {
	return &fakeHost{screens: map[string]ops.Screen{}, options: map[string]string{}, keysAfter: -1}
}

func (h *fakeHost) addSession(name, command string) term.PaneRef {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.addLocked(name, command)
}

func (h *fakeHost) addLocked(name, command string) term.PaneRef {
	h.nextPane++
	ref := term.PaneRef{ID: "%" + strconv.Itoa(h.nextPane), PID: 1000 + h.nextPane, Command: command}
	h.sessions = append(h.sessions, term.Session{
		Name: name, Created: time.Date(2026, 11, 9, 11, 0, 0, 0, time.UTC), Windows: 1,
		Panes: []term.Pane{{Ref: ref, Window: 0, Index: 0, Active: true}},
	})
	return ref
}

func (h *fakeHost) setScreen(ref term.PaneRef, lines ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.screens[ref.ID] = ops.Screen{Width: 80, Height: 24, Lines: lines}
}

func (h *fakeHost) pastes() []pasteCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]pasteCall(nil), h.pasted...)
}

func (h *fakeHost) eventLog() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...)
}

func (h *fakeHost) keys() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]string(nil), h.keysSent...)
}

func (h *fakeHost) ListSessions(context.Context) ([]term.Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.listErr != nil {
		return nil, h.listErr
	}
	return append([]term.Session(nil), h.sessions...), nil
}

func (h *fakeHost) ResolvePane(_ context.Context, t ops.Target) (term.PaneRef, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resolveCall++
	if h.resolveHook != nil {
		if ref, ok := h.resolveHook(h.resolveCall, t); ok {
			return ref, nil
		}
	}
	for _, s := range h.sessions {
		if s.Name != t.SessionName {
			continue
		}
		if t.Pane == "" {
			if p, ok := s.ActivePane(); ok {
				return p.Ref, nil
			}
		}
		for _, p := range s.Panes {
			if t.Pane == fmt.Sprintf("%d.%d", p.Window, p.Index) {
				return p.Ref, nil
			}
		}
	}
	return term.PaneRef{}, fmt.Errorf("session %q: %w", t.SessionName, term.ErrNotFound)
}

func (h *fakeHost) Snapshot(_ context.Context, p term.PaneRef) (ops.Screen, error) {
	h.mu.Lock()
	hook := h.onSnapshot
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.snapshots++
	return h.screens[p.ID], nil
}

func (h *fakeHost) Capture(_ context.Context, p term.PaneRef, lines int) (ops.Screen, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return ops.Screen{Lines: []string{"capture " + strconv.Itoa(lines)}}, nil
}

func (h *fakeHost) PasteText(_ context.Context, p term.PaneRef, text string, expect term.PaneRef) error {
	h.mu.Lock()
	hook, err := h.onPaste, h.pasteErr
	if err == nil {
		h.pasted = append(h.pasted, pasteCall{Pane: p.ID, Text: text})
		h.events = append(h.events, "paste "+p.ID)
	}
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (h *fakeHost) SendKeys(_ context.Context, p term.PaneRef, keys []string, expect term.PaneRef) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.keysErr != nil {
		n := h.keysAfter
		if n < 0 {
			n = 0
		}
		h.keysSent = append(h.keysSent, append([]string(nil), keys[:n]...))
		if n > 0 {
			h.events = append(h.events, "keys "+p.ID+" "+strings.Join(keys[:n], ","))
		}
		return n, h.keysErr
	}
	h.keysSent = append(h.keysSent, append([]string(nil), keys...))
	h.events = append(h.events, "keys "+p.ID+" "+strings.Join(keys, ","))
	return len(keys), nil
}

func (h *fakeHost) SetOption(_ context.Context, session, key, value string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, s := range h.sessions {
		if s.Name == session {
			h.options[session+"|"+key] = value
			if key == "@minion_task" {
				h.sessions[i].Task = value
			}
			return nil
		}
	}
	return fmt.Errorf("session %q: %w", session, term.ErrNotFound)
}

func (h *fakeHost) CreateSession(_ context.Context, name, cwd string, argv []string) (term.Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.createCwd, h.createArgv = cwd, argv
	if h.createRace != "" {
		h.addLocked(h.createRace, "bash")
		h.createRace = ""
	}
	if h.createErr != nil {
		if h.createAfter {
			h.addLocked(name, "bash")
		}
		if h.createAfter || h.createGone {
			h.creates++
		}
		return term.Session{}, h.createErr
	}
	h.creates++
	h.addLocked(name, "bash")
	return h.sessions[len(h.sessions)-1], nil
}

func mustJSON(t testing.TB, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeClassifier answers by the pane's command.
type fakeClassifier struct {
	mu     sync.Mutex
	byCmd  map[string]agentdetect.Result
	inputs []agentdetect.Input
}

func newFakeClassifier() *fakeClassifier {
	return &fakeClassifier{byCmd: map[string]agentdetect.Result{}}
}

func (c *fakeClassifier) set(cmd, kind, state string, choices ...agentdetect.Choice) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byCmd[cmd] = agentdetect.Result{Kind: kind, State: state, Choices: choices}
}

func (c *fakeClassifier) Classify(_ context.Context, in agentdetect.Input) agentdetect.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inputs = append(c.inputs, in)
	if r, ok := c.byCmd[in.Command]; ok {
		return r
	}
	return agentdetect.Result{Kind: ops.PaneKindOther, State: ops.PaneStateUnknown}
}

// env is an executor over fakes and a real store in a temp dir.
type env struct {
	t      *testing.T
	ex     *Executor
	host   *fakeHost
	cls    *fakeClassifier
	st     *store.Store
	clk    *fakeClock
	policy *Policy
}

var (
	allActions     = []ops.Action{ops.ActionRead, ops.ActionOperate}
	operateActions = []ops.Action{ops.ActionOperate}
	messageOnly    = []ops.Action{ops.ActionMessage}
)

func newEnv(t *testing.T, mut ...func(*Config)) *env {
	t.Helper()
	clk := newFakeClock()
	st, err := store.Open(t.TempDir(), store.WithClock(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveShare(store.ShareSelection{Mode: store.ShareAll}); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, host: newFakeHost(), cls: newFakeClassifier(), st: st, clk: clk, policy: NewPolicy(st)}
	cfg := Config{Host: e.host, Classifier: e.cls, State: st, Policy: e.policy, LockWait: 2 * time.Second}
	for _, m := range mut {
		m(&cfg)
	}
	ex, err := NewExecutor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.ex = ex
	return e
}

// call runs an op with the given actions and JSON-encodable args.
func (e *env) call(op string, actions []ops.Action, args any) (json.RawMessage, error) {
	e.t.Helper()
	var raw json.RawMessage
	if args != nil {
		raw = mustJSON(e.t, args)
	}
	return e.ex.Execute(context.Background(), Request{Op: op, Actions: actions, Args: raw})
}

// write runs a write op with all actions and decodes the WriteResult; the
// result must satisfy the action contract.
func (e *env) write(op string, args any) (ops.WriteResult, error) {
	e.t.Helper()
	return e.writeAs(op, allActions, args)
}

func (e *env) writeAs(op string, actions []ops.Action, args any) (ops.WriteResult, error) {
	e.t.Helper()
	raw, err := e.call(op, actions, args)
	if err != nil {
		return ops.WriteResult{}, err
	}
	var res ops.WriteResult
	if err := json.Unmarshal(raw, &res); err != nil {
		e.t.Fatalf("result %s is not a WriteResult: %v", raw, err)
	}
	if err := res.Validate(); err != nil {
		e.t.Fatalf("result %s breaks the action contract: %v", raw, err)
	}
	return res, nil
}

func (e *env) mustWrite(op string, args any) ops.WriteResult {
	e.t.Helper()
	res, err := e.write(op, args)
	if err != nil {
		e.t.Fatalf("%s: %v", op, err)
	}
	return res
}

func target(session string) ops.Target { return ops.Target{SessionName: session} }

func sendText(session, text string, force bool) ops.SendTextArgs {
	return ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7(), Force: force}, Target: target(session), Text: text}
}

func sendKeys(session string, force bool, keys ...string) ops.SendKeysArgs {
	return ops.SendKeysArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7(), Force: force}, Target: target(session), Keys: keys}
}

// wantAPIError asserts that err is an *APIError with the code and reason.
func wantAPIError(t *testing.T, err error, code string, reason string) {
	t.Helper()
	_ = apiErrorOf(t, err, code, reason)
}

// apiErrorOf is wantAPIError that also returns the error for further checks.
func apiErrorOf(t *testing.T, err error, code string, reason string) *APIError {
	t.Helper()
	ae, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("error = %v, want an *APIError %s", err, code)
	}
	if string(ae.Code) != code || string(ae.Reason) != reason {
		t.Fatalf("API error = %s/%s, want %s/%s (%v)", ae.Code, ae.Reason, code, reason, ae)
	}
	return ae
}

func (e *env) audit() []store.AuditRecord {
	e.t.Helper()
	rows, err := e.st.ListAudit(context.Background(), 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

var _ term.SessionHost = (*fakeHost)(nil)

// putFailState is a State whose result-cache writes can be made to fail:
// failPut refuses the record written before a request runs, failReplace the
// completed result written after it.
type putFailState struct {
	State
	failPut     atomic.Bool
	failReplace atomic.Bool
}

func (s *putFailState) PutResult(ctx context.Context, rec store.ResultRecord) error {
	if s.failPut.Load() {
		return errors.New("disk full")
	}
	return s.State.PutResult(ctx, rec)
}

func (s *putFailState) ReplaceResult(ctx context.Context, rec store.ResultRecord) error {
	if s.failReplace.Load() {
		return errors.New("disk full")
	}
	return s.State.ReplaceResult(ctx, rec)
}

// mutatePane changes the pane with the given id in place.
func (h *fakeHost) mutatePane(id string, f func(p *term.Pane)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.sessions {
		for j := range h.sessions[i].Panes {
			if h.sessions[i].Panes[j].Ref.ID == id {
				f(&h.sessions[i].Panes[j])
				return
			}
		}
	}
}

func agentChoice(label string, keys ...string) agentdetect.Choice {
	return agentdetect.Choice{Label: label, Keys: keys}
}
