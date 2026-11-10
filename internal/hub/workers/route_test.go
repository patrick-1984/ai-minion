package workers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// --- fakes ---

type fakeUsers map[string]authz.UserState

func (f fakeUsers) User(_ context.Context, id string) (authz.UserState, error) {
	u, ok := f[id]
	if !ok {
		return authz.UserState{}, authz.ErrNotFound
	}
	return u, nil
}

// fakeDir is a Directory that also answers the authz owner lookup, so that
// the two always agree.
type fakeDir struct {
	mu   sync.Mutex
	recs []WorkerRecord
	err  error
}

func (d *fakeDir) Workers(context.Context) ([]WorkerRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]WorkerRecord(nil), d.recs...), d.err
}

func (d *fakeDir) WorkerOwner(_ context.Context, id string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, w := range d.recs {
		if w.ID == id {
			return w.OwnerID, nil
		}
	}
	return "", authz.ErrNotFound
}

type scopeFunc func(authz.ResolvedResource) []ops.Action

func (f scopeFunc) Actions(r authz.ResolvedResource) []ops.Action { return f(r) }

type readItem struct {
	body json.RawMessage
	err  error
}

// fakeStream is an OpStream with scripted answers.
type fakeStream struct {
	mu       sync.Mutex
	written  []json.RawMessage
	writeErr error
	reads    []readItem
	// hang makes ReadFrame block until the stream is reset once the script
	// is used up; without it the script ends in io.EOF.
	hang   bool
	closed bool
	reset  chan struct{}
	once   sync.Once
}

func newFakeStream(reads ...readItem) *fakeStream {
	return &fakeStream{reads: reads, reset: make(chan struct{})}
}

func (s *fakeStream) WriteFrame(v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.written = append(s.written, b)
	return nil
}

func (s *fakeStream) ReadFrame() (json.RawMessage, error) {
	s.mu.Lock()
	if len(s.reads) > 0 {
		it := s.reads[0]
		s.reads = s.reads[1:]
		s.mu.Unlock()
		return it.body, it.err
	}
	hang := s.hang
	s.mu.Unlock()
	if hang {
		<-s.reset
		return nil, io.ErrClosedPipe
	}
	return nil, io.EOF
}

func (s *fakeStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func (s *fakeStream) Reset() { s.once.Do(func() { close(s.reset) }) }

func (s *fakeStream) wasReset() bool {
	select {
	case <-s.reset:
		return true
	default:
		return false
	}
}

func (s *fakeStream) request(t *testing.T) OpRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.written) != 1 {
		t.Fatalf("%d frames written, want exactly one op.request", len(s.written))
	}
	var r OpRequest
	if err := json.Unmarshal(s.written[0], &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func frame(t *testing.T, v any) readItem {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return readItem{body: b}
}

func resultFrame(t *testing.T, v any) readItem {
	return frame(t, map[string]any{"type": proto.TypeOpResult, "result": v})
}

func progressFrame(t *testing.T, v any) readItem {
	return frame(t, map[string]any{"type": proto.TypeOpProgress, "data": v})
}

func errorFrame(t *testing.T, code proto.Code) readItem {
	return frame(t, proto.NewError(code, "refused").Frame())
}

// fakeBackend serves one scripted stream per OpenOp call.
type fakeBackend struct {
	mu      sync.Mutex
	online  map[string]Online
	streams []*fakeStream
	opens   []string
	openErr error
}

func (b *fakeBackend) Get(id string) (Online, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.online[id]
	return o, ok
}

func (b *fakeBackend) OpenOp(_ context.Context, workerID, op string, _ ...string) (OpStream, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opens = append(b.opens, workerID+" "+op)
	if b.openErr != nil {
		return nil, b.openErr
	}
	if len(b.streams) == 0 {
		return nil, errors.New("no scripted stream")
	}
	s := b.streams[0]
	b.streams = b.streams[1:]
	return s, nil
}

func (b *fakeBackend) openCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.opens)
}

// --- fixtures ---

var (
	idAlice = "user-alice"
	idBob   = "user-bob"
	idRoot  = "user-root"

	wAlicePi1 = WorkerRecord{ID: ops.NewUUIDv7(), Name: "pi-1", OwnerID: idAlice, Kind: "private"}
	wAlicePi2 = WorkerRecord{ID: ops.NewUUIDv7(), Name: "pi-2", OwnerID: idAlice, Kind: "private"}
	wBobPi1   = WorkerRecord{ID: ops.NewUUIDv7(), Name: "pi-1", OwnerID: idBob, Kind: "private"}
	wBobBox   = WorkerRecord{ID: ops.NewUUIDv7(), Name: "box", OwnerID: idBob, Kind: "infrastructure"}
	wRootLab  = WorkerRecord{ID: ops.NewUUIDv7(), Name: "lab", OwnerID: idRoot, Kind: "infrastructure"}
)

var ownerNames = map[string]string{idAlice: "alice", idBob: "bob", idRoot: "root"}

type rig struct {
	rt  *Router
	dir *fakeDir
	be  *fakeBackend
}

func newRig(t *testing.T, mutate func(*RouterConfig)) *rig {
	t.Helper()
	dir := &fakeDir{recs: []WorkerRecord{wAlicePi1, wAlicePi2, wBobPi1, wBobBox, wRootLab}}
	be := &fakeBackend{online: map[string]Online{}}
	az := authz.New(fakeUsers{
		idAlice: {Role: authz.RoleUser, Active: true},
		idBob:   {Role: authz.RoleUser, Active: true},
		idRoot:  {Role: authz.RoleAdmin, Active: true},
	}, dir)
	cfg := RouterConfig{
		Backend: be, Directory: dir, Authz: az,
		OwnerName: func(_ context.Context, id string) string { return ownerNames[id] },
		Logger:    slog.New(slog.DiscardHandler),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	rt, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &rig{rt: rt, dir: dir, be: be}
}

func (r *rig) script(streams ...*fakeStream) { r.be.streams = streams }

func routeAlice() authz.Principal { return authz.NewSessionPrincipal(idAlice) }
func routeRoot() authz.Principal  { return authz.NewSessionPrincipal(idRoot) }

func code(err error) proto.Code { return proto.CodeOf(err) }

// --- resolver ---

// errInvalidTarget stands for an ops argument error (the API maps it to
// invalid_argument), which is not a proto error.
const errInvalidTarget proto.Code = "ops.ErrInvalidArgument"

func TestResolveTargetTable(t *testing.T) {
	r := newRig(t, nil)
	actAs := authz.NewActAsPrincipal(idRoot, idAlice, time.Now().Add(time.Hour))
	apiKeyPi2Only := authz.NewAPIKeyPrincipal(idAlice, scopeFunc(func(res authz.ResolvedResource) []ops.Action {
		if res.WorkerID == wAlicePi2.ID {
			return []ops.Action{ops.ActionRead}
		}
		return nil
	}), "")

	// Alice also owns a Worker whose name contains "/" and one whose name is
	// as long as a Worker name may be.
	wAliceSlash := WorkerRecord{ID: ops.NewUUIDv7(), Name: "bob/pi-1", OwnerID: idAlice, Kind: "private"}
	longName := strings.Repeat("x", 64)
	wAliceLong := WorkerRecord{ID: ops.NewUUIDv7(), Name: longName, OwnerID: idAlice, Kind: "private"}
	r.dir.recs = append(r.dir.recs, wAliceSlash, wAliceLong)
	sessionOnlyKey := authz.NewAPIKeyPrincipal(idAlice, scopeFunc(func(res authz.ResolvedResource) []ops.Action {
		if res.Kind == authz.ResourceSession && res.WorkerID == wAlicePi1.ID && res.SessionName == "minion-1" {
			return []ops.Action{ops.ActionRead}
		}
		return nil
	}), "")

	tests := []struct {
		name     string
		p        authz.Principal
		target   string
		all      bool
		want     ops.Target
		wantCode proto.Code
	}{
		{"admin all: a literal name with a slash does not hide the owner's worker", routeRoot(), "bob/pi-1:s", true, ops.Target{}, proto.CodeConflict},
		{"a user's own worker whose name has a slash", routeAlice(), "bob/pi-1:s", false, ops.Target{WorkerID: wAliceSlash.ID, SessionName: "s"}, ""},
		{"64-byte worker name behind an owner name", routeRoot(), "alice/" + longName + ":s", true, ops.Target{WorkerID: wAliceLong.ID, SessionName: "s"}, ""},
		{"64-byte worker name behind an owner id", routeRoot(), idAlice + "/" + longName + ":s", true, ops.Target{WorkerID: wAliceLong.ID, SessionName: "s"}, ""},
		{"64-byte worker name alone", routeAlice(), longName + ":s", false, ops.Target{WorkerID: wAliceLong.ID, SessionName: "s"}, ""},
		{"key scoped to one session resolves that session", sessionOnlyKey, "pi-1:minion-1", false, ops.Target{WorkerID: wAlicePi1.ID, SessionName: "minion-1"}, ""},
		{"key scoped to one session does not see another session", sessionOnlyKey, "pi-1:minion-2", false, ops.Target{}, proto.CodeTargetNotFound},
		{"key scoped to one session does not see another worker", sessionOnlyKey, "pi-2:minion-1", false, ops.Target{}, proto.CodeTargetNotFound},
		{"own worker", routeAlice(), "pi-1:minion-3", false, ops.Target{WorkerID: wAlicePi1.ID, SessionName: "minion-3"}, ""},
		{"own worker with pane", routeAlice(), "pi-2:minion-3:0.1", false, ops.Target{WorkerID: wAlicePi2.ID, SessionName: "minion-3", Pane: "0.1"}, ""},
		{"same name, other owner: the caller's space wins", func() authz.Principal { return authz.NewSessionPrincipal(idBob) }(), "pi-1:s", false, ops.Target{WorkerID: wBobPi1.ID, SessionName: "s"}, ""},
		{"foreign worker looks like a missing one", routeAlice(), "box:minion-1", false, ops.Target{}, proto.CodeTargetNotFound},
		{"foreign worker with all=true: a user still cannot see it", routeAlice(), "box:minion-1", true, ops.Target{}, proto.CodeTargetNotFound},
		{"missing worker", routeAlice(), "nope:minion-1", false, ops.Target{}, proto.CodeTargetNotFound},
		{"admin mine: own worker", routeRoot(), "lab:minion-1", false, ops.Target{WorkerID: wRootLab.ID, SessionName: "minion-1"}, ""},
		{"admin mine: foreign worker is not visible", routeRoot(), "box:minion-1", false, ops.Target{}, proto.CodeTargetNotFound},
		{"admin all: foreign worker with a unique name", routeRoot(), "box:minion-1", true, ops.Target{WorkerID: wBobBox.ID, SessionName: "minion-1"}, ""},
		{"admin all: ambiguous name is a conflict", routeRoot(), "pi-1:minion-1", true, ops.Target{}, proto.CodeConflict},
		{"admin all: owner/worker disambiguates", routeRoot(), "alice/pi-1:minion-1", true, ops.Target{WorkerID: wAlicePi1.ID, SessionName: "minion-1"}, ""},
		{"admin all: owner name is case-insensitive", routeRoot(), "BOB/pi-1:minion-1", true, ops.Target{WorkerID: wBobPi1.ID, SessionName: "minion-1"}, ""},
		{"admin all: owner id disambiguates", routeRoot(), idBob + "/pi-1:minion-1:1.0", true, ops.Target{WorkerID: wBobPi1.ID, SessionName: "minion-1", Pane: "1.0"}, ""},
		{"admin all: unknown owner", routeRoot(), "carol/pi-1:minion-1", true, ops.Target{}, proto.CodeTargetNotFound},
		{"admin all: right owner, wrong worker", routeRoot(), "alice/box:minion-1", true, ops.Target{}, proto.CodeTargetNotFound},
		{"worker id instead of the name", routeAlice(), wAlicePi2.ID + ":minion-9", false, ops.Target{WorkerID: wAlicePi2.ID, SessionName: "minion-9"}, ""},
		{"foreign worker id", routeAlice(), wBobBox.ID + ":minion-9", false, ops.Target{}, proto.CodeTargetNotFound},
		{"act-as resolves in the target user's space", actAs, "pi-2:minion-1", false, ops.Target{WorkerID: wAlicePi2.ID, SessionName: "minion-1"}, ""},
		{"act-as does not see the admin's own workers", actAs, "lab:minion-1", false, ops.Target{}, proto.CodeTargetNotFound},
		{"scoped key sees only what its scope covers", apiKeyPi2Only, "pi-1:minion-1", false, ops.Target{}, proto.CodeTargetNotFound},
		{"scoped key: covered worker", apiKeyPi2Only, "pi-2:minion-1", false, ops.Target{WorkerID: wAlicePi2.ID, SessionName: "minion-1"}, ""},
		{"malformed target", routeAlice(), "pi-1", false, ops.Target{}, errInvalidTarget},
		{"malformed session", routeAlice(), "pi-1:a.b", false, ops.Target{}, errInvalidTarget},
		{"deactivated or unknown principal sees nothing", authz.NewSessionPrincipal("ghost"), "pi-1:s", false, ops.Target{}, proto.CodeTargetNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.rt.Resolve(context.Background(), tc.p, tc.target, tc.all)
			if tc.wantCode == errInvalidTarget {
				if !errors.Is(err, ops.ErrInvalidArgument) {
					t.Fatalf("err = %v, want an invalid argument", err)
				}
				return
			}
			if tc.wantCode != "" {
				if err == nil || code(err) != tc.wantCode {
					t.Fatalf("err = %v (%s), want code %s", err, code(err), tc.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("target = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestResolveConflictNamesTheQualifiedForms(t *testing.T) {
	r := newRig(t, nil)
	_, err := r.rt.Resolve(context.Background(), routeRoot(), "pi-1:minion-1", true)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeConflict {
		t.Fatalf("err = %v", err)
	}
	var d struct {
		Candidates []string `json:"candidates"`
	}
	if err := json.Unmarshal(pe.Details, &d); err != nil {
		t.Fatal(err)
	}
	if want := []string{"alice/pi-1", "bob/pi-1"}; !reflect.DeepEqual(d.Candidates, want) {
		t.Errorf("candidates = %v, want %v", d.Candidates, want)
	}
}

func TestResolveSlashConflictNamesBothWorkers(t *testing.T) {
	r := newRig(t, nil)
	r.dir.recs = append(r.dir.recs, WorkerRecord{ID: ops.NewUUIDv7(), Name: "bob/pi-1", OwnerID: idAlice, Kind: "private"})
	_, err := r.rt.Resolve(context.Background(), routeRoot(), "bob/pi-1:s", true)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeConflict {
		t.Fatalf("err = %v", err)
	}
	var d struct {
		Candidates []string `json:"candidates"`
	}
	if err := json.Unmarshal(pe.Details, &d); err != nil {
		t.Fatal(err)
	}
	if want := []string{"alice/bob/pi-1", "bob/pi-1"}; !reflect.DeepEqual(d.Candidates, want) {
		t.Errorf("candidates = %v, want %v", d.Candidates, want)
	}
}

func TestResolveWithoutOwnerNamesUsesIds(t *testing.T) {
	r := newRig(t, func(c *RouterConfig) { c.OwnerName = nil })
	got, err := r.rt.Resolve(context.Background(), routeRoot(), idAlice+"/pi-1:s", true)
	if err != nil || got.WorkerID != wAlicePi1.ID {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestResolveReadsTheDirectoryEveryTime(t *testing.T) {
	r := newRig(t, nil)
	if _, err := r.rt.Resolve(context.Background(), routeAlice(), "pi-1:s", false); err != nil {
		t.Fatal(err)
	}
	r.dir.mu.Lock()
	r.dir.recs = nil
	r.dir.mu.Unlock()
	if _, err := r.rt.Resolve(context.Background(), routeAlice(), "pi-1:s", false); code(err) != proto.CodeTargetNotFound {
		t.Errorf("a removed Worker still resolves: %v", err)
	}
	r.dir.err = errors.New("db down")
	if _, err := r.rt.Resolve(context.Background(), routeAlice(), "pi-1:s", false); err == nil || code(err) != proto.CodeInternal {
		t.Errorf("a failing directory must fail the resolution, got %v", err)
	}
}

// --- workers_list data ---

func TestListWorkersMineAllAndOnlineState(t *testing.T) {
	r := newRig(t, nil)
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	r.be.online[wAlicePi1.ID] = Online{WorkerID: wAlicePi1.ID, Version: proto.Version{Major: 1, Minor: 0}, Ops: []string{"send_text", "sessions_list"}, ConnectedAt: at}

	names := func(l []ops.WorkerInfo) []string {
		var out []string
		for _, w := range l {
			out = append(out, w.Owner+"/"+w.Name)
		}
		return out
	}

	got, err := r.rt.ListWorkers(context.Background(), routeAlice(), false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alice/pi-1", "alice/pi-2"}; !reflect.DeepEqual(names(got), want) {
		t.Errorf("user list = %v, want %v", names(got), want)
	}
	pi1 := got[0]
	if !pi1.Online || pi1.ProtoVersion != "1.0" || pi1.ConnectedAt == nil || !pi1.ConnectedAt.Time().Equal(at) ||
		!reflect.DeepEqual(pi1.Ops, []string{"send_text", "sessions_list"}) || pi1.Kind != "private" || pi1.OwnerID != idAlice || pi1.ID != wAlicePi1.ID {
		t.Errorf("online worker info = %+v", pi1)
	}
	if got[1].Online || got[1].ProtoVersion != "" || got[1].ConnectedAt != nil || got[1].Ops != nil {
		t.Errorf("offline worker info = %+v", got[1])
	}

	// A user asking for all still sees only their own Workers.
	got, err = r.rt.ListWorkers(context.Background(), routeAlice(), true)
	if err != nil || !reflect.DeepEqual(names(got), []string{"alice/pi-1", "alice/pi-2"}) {
		t.Errorf("user all=true = %v, %v", names(got), err)
	}

	// An admin defaults to "mine" and sees every Worker with all=true.
	got, err = r.rt.ListWorkers(context.Background(), routeRoot(), false)
	if err != nil || !reflect.DeepEqual(names(got), []string{"root/lab"}) {
		t.Errorf("admin mine = %v, %v", names(got), err)
	}
	got, err = r.rt.ListWorkers(context.Background(), routeRoot(), true)
	if err != nil {
		t.Fatal(err)
	}
	// Ordered by name, then owner.
	want := []string{"bob/box", "root/lab", "alice/pi-1", "bob/pi-1", "alice/pi-2"}
	if !reflect.DeepEqual(names(got), want) {
		t.Errorf("admin all = %v, want %v", names(got), want)
	}
	if got[0].Kind != "infrastructure" {
		t.Errorf("kind = %q", got[0].Kind)
	}

	// Act-as lists the target user's space.
	actAs := authz.NewActAsPrincipal(idRoot, idBob, time.Now().Add(time.Hour))
	got, err = r.rt.ListWorkers(context.Background(), actAs, true)
	if err != nil || !reflect.DeepEqual(names(got), []string{"bob/box", "bob/pi-1"}) {
		t.Errorf("act-as = %v, %v", names(got), err)
	}
}

func TestListWorkersFiltersByScopeAndGivesAnEmptyList(t *testing.T) {
	r := newRig(t, nil)
	key := authz.NewAPIKeyPrincipal(idAlice, scopeFunc(func(res authz.ResolvedResource) []ops.Action {
		if res.WorkerID == wAlicePi2.ID {
			return []ops.Action{ops.ActionRead}
		}
		return nil
	}), "")
	got, err := r.rt.ListWorkers(context.Background(), key, false)
	if err != nil || len(got) != 1 || got[0].ID != wAlicePi2.ID {
		t.Errorf("scoped list = %+v, %v", got, err)
	}
	got, err = r.rt.ListWorkers(context.Background(), authz.NewSessionPrincipal("ghost"), true)
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("unknown principal list = %#v, %v (want an empty, non-nil list)", got, err)
	}
}

// --- routing ---

func sessionsResult() map[string]any {
	return map[string]any{"sessions": []any{map[string]any{"name": "minion-1", "created": "2026-11-09T12:00:00.000Z", "windows": 1, "attached": false, "pane": nil}}}
}

func TestCallRoutesAReadWithTheEffectiveActions(t *testing.T) {
	r := newRig(t, nil)
	st := newFakeStream(progressFrame(t, map[string]any{"note": "ignored"}), resultFrame(t, sessionsResult()))
	r.script(st)

	target := ops.Target{WorkerID: wAlicePi1.ID, SessionName: "minion-1"}
	res, err := r.rt.Call(context.Background(), CallRequest{
		Principal: routeAlice(), Op: ops.OpSessionSnapshot, WorkerID: target.WorkerID, Session: target.SessionName,
		Args: ops.SessionSnapshotArgs{Target: target},
	})
	_ = res
	if err != nil {
		t.Fatal(err)
	}
	req := st.request(t)
	if req.Type != proto.TypeOpRequest || req.Op != "session_snapshot" || !ops.ValidUUIDv7(req.RequestID) {
		t.Errorf("request = %+v", req)
	}
	if want := (ops.ActionSet{ops.ActionMessage, ops.ActionOperate, ops.ActionRead}); !reflect.DeepEqual(req.Actions, want) {
		t.Errorf("actions = %v, want the owner's %v", req.Actions, want)
	}
	var args ops.SessionSnapshotArgs
	if err := json.Unmarshal(req.Args, &args); err != nil || args.Target != target {
		t.Errorf("args = %s (%v)", req.Args, err)
	}
	if !st.closed {
		t.Error("the stream was not closed after the result")
	}
	if r.be.openCount() != 1 || r.be.opens[0] != wAlicePi1.ID+" session_snapshot" {
		t.Errorf("opens = %v", r.be.opens)
	}
}

func TestCallDecodesTheResultIntoTheOpsResultType(t *testing.T) {
	r := newRig(t, nil)
	r.script(newFakeStream(resultFrame(t, sessionsResult())))
	res, err := r.rt.Call(context.Background(), CallRequest{
		Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: wAlicePi1.ID, Args: ops.SessionsListArgs{},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := res.(ops.SessionsListResult)
	if !ok || len(got.Sessions) != 1 || got.Sessions[0].Name != "minion-1" {
		t.Fatalf("result = %#v", res)
	}
}

func TestCallRoutesAWriteWithItsRequestIDAndActions(t *testing.T) {
	r := newRig(t, nil)
	id := ops.NewUUIDv7()
	st := newFakeStream(resultFrame(t, ops.WriteResult{Outcome: ops.OutcomeDelivered}))
	r.script(st)
	target := ops.Target{WorkerID: wAlicePi1.ID, SessionName: "minion-3"}
	res, err := r.rt.Call(context.Background(), CallRequest{
		Principal: routeAlice(), Op: ops.OpSendText, WorkerID: target.WorkerID, Session: target.SessionName,
		Args: ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: id, Force: true}, Target: target, Text: "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wr, ok := res.(ops.WriteResult); !ok || wr.Outcome != ops.OutcomeDelivered {
		t.Fatalf("result = %#v", res)
	}
	req := st.request(t)
	if req.RequestID != id || req.Op != "send_text" {
		t.Errorf("request = %+v", req)
	}
	var sent map[string]any
	if err := json.Unmarshal(req.Args, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["request_id"] != id || sent["force"] != true || sent["text"] != "hello" {
		t.Errorf("args = %v", sent)
	}
}

func TestAdminActionsIncludeManageAndAKeyGetsItsScope(t *testing.T) {
	r := newRig(t, nil)
	st := newFakeStream(resultFrame(t, sessionsResult()))
	r.script(st)
	if _, err := r.rt.Call(context.Background(), CallRequest{Principal: routeRoot(), Op: ops.OpSessionsList, WorkerID: wBobBox.ID, Args: ops.SessionsListArgs{}}); err != nil {
		t.Fatal(err)
	}
	want := ops.ActionSet{ops.ActionManage, ops.ActionMessage, ops.ActionOperate, ops.ActionRead}
	if got := st.request(t).Actions; !reflect.DeepEqual(got, want) {
		t.Errorf("admin actions = %v, want %v", got, want)
	}

	readOnly := authz.NewAPIKeyPrincipal(idAlice, scopeFunc(func(authz.ResolvedResource) []ops.Action {
		return []ops.Action{ops.ActionRead}
	}), "")
	st = newFakeStream(resultFrame(t, sessionsResult()))
	r.script(st)
	if _, err := r.rt.Call(context.Background(), CallRequest{Principal: readOnly, Op: ops.OpSessionsList, WorkerID: wAlicePi1.ID, Args: ops.SessionsListArgs{}}); err != nil {
		t.Fatal(err)
	}
	if got := st.request(t).Actions; !reflect.DeepEqual(got, ops.ActionSet{ops.ActionRead}) {
		t.Errorf("key actions = %v, want only read", got)
	}
}

func TestCallDeniesBeforeAnythingIsOpened(t *testing.T) {
	r := newRig(t, nil)
	target := ops.Target{WorkerID: wBobBox.ID, SessionName: "minion-1"}
	req := func(p authz.Principal, op string, args any) CallRequest {
		return CallRequest{Principal: p, Op: op, WorkerID: target.WorkerID, Session: target.SessionName, Args: args}
	}
	write := ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target, Text: "x"}

	// A foreign Worker: not_found, no existence leak.
	if _, err := r.rt.Call(context.Background(), req(routeAlice(), ops.OpSessionCapture, ops.SessionCaptureArgs{Target: target})); code(err) != proto.CodeNotFound {
		t.Errorf("foreign read: %v", err)
	}
	if _, err := r.rt.Call(context.Background(), req(routeAlice(), ops.OpSendText, write)); code(err) != proto.CodeNotFound {
		t.Errorf("foreign write: %v", err)
	}
	// A key limited to read cannot write on the owner's own Worker.
	readOnly := authz.NewAPIKeyPrincipal(idBob, scopeFunc(func(authz.ResolvedResource) []ops.Action {
		return []ops.Action{ops.ActionRead}
	}), "")
	if _, err := r.rt.Call(context.Background(), req(readOnly, ops.OpSendText, write)); code(err) != proto.CodeForbidden {
		t.Errorf("read-only key write: %v", err)
	}
	// An unknown Worker.
	missing := CallRequest{Principal: routeRoot(), Op: ops.OpSessionsList, WorkerID: ops.NewUUIDv7(), Args: ops.SessionsListArgs{}}
	if _, err := r.rt.Call(context.Background(), missing); code(err) != proto.CodeNotFound {
		t.Errorf("unknown Worker: %v", err)
	}
	if r.be.openCount() != 0 {
		t.Errorf("a denied call opened %d streams", r.be.openCount())
	}
}

func TestCallRefusesWhatIsNotAWorkerCall(t *testing.T) {
	r := newRig(t, nil)
	wid := wAlicePi1.ID
	tests := []struct {
		name string
		req  CallRequest
		want proto.Code
	}{
		{"unknown op", CallRequest{Principal: routeAlice(), Op: "nope", WorkerID: wid}, proto.CodeUnsupportedOp},
		{"hub op", CallRequest{Principal: routeAlice(), Op: ops.OpWorkersList, WorkerID: wid}, proto.CodeUnsupportedOp},
		{"no worker", CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, Args: ops.SessionsListArgs{}}, proto.CodeInvalidArgument},
		{"write without request_id", CallRequest{Principal: routeAlice(), Op: ops.OpSetTask, WorkerID: wid, Session: "s", Args: ops.SetTaskArgs{Target: ops.Target{WorkerID: wid, SessionName: "s"}}}, proto.CodeInvalidArgument},
		{"write with nil args", CallRequest{Principal: routeAlice(), Op: ops.OpSetTask, WorkerID: wid, Session: "s"}, proto.CodeInvalidArgument},
		{"stream op through Call", CallRequest{Principal: routeAlice(), Op: ops.OpPipeWait, WorkerID: wid, Session: "s", Args: ops.PipeWaitArgs{}}, proto.CodeInternal},
	}
	for _, tc := range tests {
		if _, err := r.rt.Call(context.Background(), tc.req); code(err) != tc.want {
			t.Errorf("%s: err = %v, want %s", tc.name, err, tc.want)
		}
	}
	err := r.rt.Stream(context.Background(), CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: wid}, func(any) error { return nil })
	if code(err) != proto.CodeInternal {
		t.Errorf("non-stream op through Stream: %v", err)
	}
	if r.be.openCount() != 0 {
		t.Errorf("%d streams opened", r.be.openCount())
	}
}

func TestOfflineWorkerIsWorkerOffline(t *testing.T) {
	// The real registry: a Worker that is not connected.
	reg := NewRegistry()
	r := newRig(t, func(c *RouterConfig) { c.Backend = RegistryBackend(reg) })
	_, err := r.rt.Call(context.Background(), CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: wAlicePi1.ID, Args: ops.SessionsListArgs{}})
	if code(err) != proto.CodeWorkerOffline {
		t.Fatalf("err = %v", err)
	}
	// A write to an offline Worker is a plain error, not an outcome: nothing was sent.
	target := ops.Target{WorkerID: wAlicePi1.ID, SessionName: "s"}
	res, err := r.rt.Call(context.Background(), CallRequest{
		Principal: routeAlice(), Op: ops.OpSendKeys, WorkerID: target.WorkerID, Session: "s",
		Args: ops.SendKeysArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target, Keys: []string{"Enter"}},
	})
	if code(err) != proto.CodeWorkerOffline || res != nil {
		t.Fatalf("write to an offline Worker: %#v, %v", res, err)
	}
}

func TestUnannouncedOpIsUnsupportedWithoutOpeningAStream(t *testing.T) {
	reg := NewRegistry()
	sess := newFakeSession()
	reg.Add(Online{WorkerID: wAlicePi1.ID, Name: "pi-1", OwnerID: idAlice, Kind: "private", Ops: []string{"send_text"}}, sess)
	r := newRig(t, func(c *RouterConfig) { c.Backend = RegistryBackend(reg) })
	_, err := r.rt.Call(context.Background(), CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: wAlicePi1.ID, Args: ops.SessionsListArgs{}})
	if code(err) != proto.CodeUnsupportedOp {
		t.Fatalf("err = %v", err)
	}
	if sess.openCount() != 0 {
		t.Errorf("a stream was opened for an op the Worker did not announce")
	}
}

func writeReq(t *testing.T, id string) CallRequest {
	target := ops.Target{WorkerID: wAlicePi1.ID, SessionName: "minion-3"}
	return CallRequest{
		Principal: routeAlice(), Op: ops.OpSendText, WorkerID: target.WorkerID, Session: target.SessionName,
		Args: ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: id}, Target: target, Text: "hello"},
	}
}

func requireUnknown(t *testing.T, res any, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("an unknown write is a result, not an error: %v", err)
	}
	wr, ok := res.(ops.WriteResult)
	if !ok {
		t.Fatalf("result = %#v", res)
	}
	if wr.Outcome != ops.OutcomeUnknown || wr.Reason != string(proto.CodeWorkerOffline) || wr.Forceable || wr.Forced {
		t.Errorf("result = %+v", wr)
	}
	if len(wr.Warnings) != 1 || wr.Warnings[0].Code != ops.WarnNoAnswer {
		t.Errorf("warnings = %+v", wr.Warnings)
	}
	if err := wr.Validate(); err != nil {
		t.Errorf("the unknown result violates the action contract: %v", err)
	}
}

func TestDroppedStreamDuringAWriteIsUnknownAndNeverRetried(t *testing.T) {
	tests := []struct {
		name string
		st   *fakeStream
	}{
		{"closed before any answer", newFakeStream()},
		{"connection error", newFakeStream(readItem{err: io.ErrUnexpectedEOF})},
		{"progress, then closed", newFakeStream(progressFrame(t, "x"))},
		{"oversized frame from the Worker", newFakeStream(readItem{err: proto.NewError(proto.CodeFrameTooLarge, "big")})},
		{"garbage frame", newFakeStream(readItem{body: json.RawMessage(`[1]`)})},
		{"request could not be written", func() *fakeStream {
			s := newFakeStream()
			s.writeErr = io.ErrClosedPipe
			return s
		}()},
		{"result is not an object", newFakeStream(resultFrame(t, "text"))},
		{"result without a body", newFakeStream(frame(t, map[string]any{"type": proto.TypeOpResult}))},
		{"result breaks the action contract", newFakeStream(resultFrame(t, map[string]any{"outcome": "bogus"}))},
		{"failed_not_sent without a reason", newFakeStream(resultFrame(t, map[string]any{"outcome": "failed_not_sent"}))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, nil)
			r.script(tc.st, newFakeStream(resultFrame(t, ops.WriteResult{Outcome: ops.OutcomeDelivered})))
			res, err := r.rt.Call(context.Background(), writeReq(t, ops.NewUUIDv7()))
			requireUnknown(t, res, err)
			if r.be.openCount() != 1 {
				t.Errorf("the write was opened %d times; a write is never retried", r.be.openCount())
			}
			if !tc.st.wasReset() && !tc.st.closed {
				t.Error("the broken stream was left open")
			}
		})
	}
}

func TestOversizedRequestIsRefusedAsAnErrorNotAnOutcome(t *testing.T) {
	r := newRig(t, nil)
	st := newFakeStream()
	st.writeErr = proto.NewError(proto.CodeFrameTooLarge, "too large")
	r.script(st)
	res, err := r.rt.Call(context.Background(), writeReq(t, ops.NewUUIDv7()))
	if code(err) != proto.CodeFrameTooLarge || res != nil {
		t.Fatalf("res = %#v, err = %v: nothing was written, so this is not an unknown outcome", res, err)
	}
}

func TestWorkerErrorFrameIsReturnedAsTheSameCode(t *testing.T) {
	for _, c := range []proto.Code{proto.CodePreviewOnly, proto.CodeNotShared, proto.CodeTargetNotFound, proto.CodeForbidden} {
		r := newRig(t, nil)
		r.script(newFakeStream(errorFrame(t, c)))
		res, err := r.rt.Call(context.Background(), writeReq(t, ops.NewUUIDv7()))
		if res != nil || code(err) != c {
			t.Errorf("code %s: res = %#v err = %v", c, res, err)
		}
	}
	// An error before the result of a read is the same.
	r := newRig(t, nil)
	r.script(newFakeStream(progressFrame(t, 1), errorFrame(t, proto.CodeNotShared)))
	_, err := r.rt.Call(context.Background(), CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: wAlicePi1.ID, Args: ops.SessionsListArgs{}})
	if code(err) != proto.CodeNotShared {
		t.Errorf("read error frame: %v", err)
	}
}

func TestDroppedStreamDuringAReadIsWorkerOffline(t *testing.T) {
	r := newRig(t, nil)
	st := newFakeStream()
	r.script(st, newFakeStream(resultFrame(t, sessionsResult())))
	_, err := r.rt.Call(context.Background(), CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: wAlicePi1.ID, Args: ops.SessionsListArgs{}})
	if code(err) != proto.CodeWorkerOffline {
		t.Fatalf("err = %v", err)
	}
	if r.be.openCount() != 1 {
		t.Errorf("a read was retried (%d opens)", r.be.openCount())
	}
	// A result a read cannot decode is an internal error.
	r = newRig(t, nil)
	r.script(newFakeStream(resultFrame(t, "text")))
	_, err = r.rt.Call(context.Background(), CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: wAlicePi1.ID, Args: ops.SessionsListArgs{}})
	if code(err) != proto.CodeInternal {
		t.Errorf("unreadable read result: %v", err)
	}
}

func TestTimeoutAndCancelDuringAWriteAreUnknown(t *testing.T) {
	r := newRig(t, func(c *RouterConfig) { c.ResultTimeout = 30 * time.Millisecond })
	st := newFakeStream()
	st.hang = true
	r.script(st)
	res, err := r.rt.Call(context.Background(), writeReq(t, ops.NewUUIDv7()))
	requireUnknown(t, res, err)
	if !st.wasReset() {
		t.Error("the stream of a timed-out write was not abandoned")
	}

	// The caller going away is the same.
	r = newRig(t, nil)
	st = newFakeStream()
	st.hang = true
	r.script(st)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	res, err = r.rt.Call(ctx, writeReq(t, ops.NewUUIDv7()))
	requireUnknown(t, res, err)

	// A read that times out is worker_offline.
	r = newRig(t, func(c *RouterConfig) { c.ResultTimeout = 30 * time.Millisecond })
	st = newFakeStream()
	st.hang = true
	r.script(st)
	_, err = r.rt.Call(context.Background(), CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: wAlicePi1.ID, Args: ops.SessionsListArgs{}})
	if code(err) != proto.CodeWorkerOffline {
		t.Errorf("read timeout: %v", err)
	}
}

func TestOpenFailureIsNotAnOutcome(t *testing.T) {
	r := newRig(t, nil)
	r.be.openErr = errors.New("yamux: broken")
	res, err := r.rt.Call(context.Background(), writeReq(t, ops.NewUUIDv7()))
	if res != nil || code(err) != proto.CodeWorkerOffline {
		t.Errorf("res = %#v, err = %v (nothing was sent: a plain error)", res, err)
	}
}

func TestStreamRelaysElementsInOrder(t *testing.T) {
	r := newRig(t, nil)
	turn := func(text string) map[string]any {
		return map[string]any{"turn_id": ops.NewUUIDv7(), "session": "minion-1", "direction": "received", "text": text, "at": "2026-11-09T12:00:00.000Z"}
	}
	st := newFakeStream(progressFrame(t, turn("one")), progressFrame(t, turn("two")), frame(t, map[string]any{"type": proto.TypeOpResult}))
	r.script(st)
	var got []string
	target := ops.Target{WorkerID: wAlicePi1.ID, SessionName: "minion-1"}
	err := r.rt.Stream(context.Background(), CallRequest{
		Principal: routeAlice(), Op: ops.OpPipeWait, WorkerID: target.WorkerID, Session: target.SessionName,
		Args: ops.PipeWaitArgs{Target: target},
	}, func(v any) error {
		got = append(got, v.(ops.PipeTurn).Text)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Errorf("elements = %v", got)
	}
	if req := st.request(t); req.Op != "pipe_wait" || !reflect.DeepEqual(req.Actions, ops.ActionSet{ops.ActionMessage, ops.ActionOperate, ops.ActionRead}) {
		t.Errorf("request = %+v", req)
	}
}

func TestStreamFailures(t *testing.T) {
	turn := map[string]any{"turn_id": ops.NewUUIDv7(), "session": "s", "direction": "sent", "text": "x", "at": "2026-11-09T12:00:00.000Z"}
	target := ops.Target{WorkerID: wAlicePi1.ID, SessionName: "s"}
	req := CallRequest{Principal: routeAlice(), Op: ops.OpPipeWait, WorkerID: target.WorkerID, Session: "s", Args: ops.PipeWaitArgs{Target: target}}

	t.Run("lost midway", func(t *testing.T) {
		r := newRig(t, nil)
		r.script(newFakeStream(progressFrame(t, turn)))
		n := 0
		err := r.rt.Stream(context.Background(), req, func(any) error { n++; return nil })
		if code(err) != proto.CodeWorkerOffline || n != 1 {
			t.Errorf("n = %d, err = %v", n, err)
		}
	})
	t.Run("emit fails", func(t *testing.T) {
		r := newRig(t, nil)
		st := newFakeStream(progressFrame(t, turn), progressFrame(t, turn))
		r.script(st)
		boom := errors.New("client gone")
		err := r.rt.Stream(context.Background(), req, func(any) error { return boom })
		if !errors.Is(err, boom) || !st.wasReset() {
			t.Errorf("err = %v, reset = %v", err, st.wasReset())
		}
	})
	t.Run("worker error", func(t *testing.T) {
		r := newRig(t, nil)
		r.script(newFakeStream(errorFrame(t, proto.CodeNotShared)))
		if err := r.rt.Stream(context.Background(), req, func(any) error { return nil }); code(err) != proto.CodeNotShared {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unreadable element", func(t *testing.T) {
		r := newRig(t, nil)
		r.script(newFakeStream(progressFrame(t, "text")))
		if err := r.rt.Stream(context.Background(), req, func(any) error { return nil }); code(err) != proto.CodeWorkerOffline {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("context ends the wait", func(t *testing.T) {
		r := newRig(t, nil)
		st := newFakeStream()
		st.hang = true
		r.script(st)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		if err := r.rt.Stream(ctx, req, func(any) error { return nil }); err == nil || !st.wasReset() {
			t.Errorf("err = %v, reset = %v", err, st.wasReset())
		}
	})
	t.Run("denied", func(t *testing.T) {
		r := newRig(t, nil)
		foreign := req
		foreign.WorkerID = wBobBox.ID
		if err := r.rt.Stream(context.Background(), foreign, func(any) error { return nil }); code(err) != proto.CodeNotFound || r.be.openCount() != 0 {
			t.Errorf("err = %v, opens = %d", err, r.be.openCount())
		}
	})
}

func TestUnknownFrameTypesAreIgnored(t *testing.T) {
	r := newRig(t, nil)
	r.script(newFakeStream(
		frame(t, map[string]any{"type": "op.future", "x": 1}),
		resultFrame(t, ops.WriteResult{Outcome: ops.OutcomeDelivered}),
	))
	res, err := r.rt.Call(context.Background(), writeReq(t, ops.NewUUIDv7()))
	if err != nil || res.(ops.WriteResult).Outcome != ops.OutcomeDelivered {
		t.Errorf("res = %#v, err = %v", res, err)
	}
}

func TestNewRouterNeedsItsDependencies(t *testing.T) {
	if _, err := NewRouter(RouterConfig{}); err == nil {
		t.Error("an empty config was accepted")
	}
	dir := &fakeDir{}
	az := authz.New(fakeUsers{}, dir)
	be := &fakeBackend{}
	for name, c := range map[string]RouterConfig{
		"no backend":   {Directory: dir, Authz: az},
		"no directory": {Backend: be, Authz: az},
		"no authz":     {Backend: be, Directory: dir},
	} {
		if _, err := NewRouter(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// --- directory on the real store ---

func TestStoreDirectoryListsTheWorkersTable(t *testing.T) {
	st := openStore(t, t.TempDir())
	now := time.Now()
	for _, w := range []struct{ name, owner, kind string }{
		{"zeta", idAlice, store.WorkerKindPrivate},
		{"alpha", idBob, store.WorkerKindInfrastructure},
	} {
		key := newKey(t)
		row := store.WorkerRow{ID: store.NewID(), Pub: key.Public(), Name: w.name, OwnerID: w.owner, Kind: w.kind, CreatedAt: now.UnixMilli()}
		if err := st.InsertWorker(context.Background(), nil, row); err != nil {
			t.Fatal(err)
		}
	}
	got, err := StoreDirectory{Store: st}.Workers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "alpha" || got[0].Kind != "infrastructure" || got[0].OwnerID != idBob ||
		got[1].Name != "zeta" || got[1].CreatedAt.UnixMilli() != now.UnixMilli() {
		t.Errorf("workers = %+v", got)
	}
	_ = st.Close()
	if _, err := (StoreDirectory{Store: st}).Workers(context.Background()); err == nil {
		t.Error("a closed database must be an error")
	}
}

// --- in-process Hub with a test Worker ---

// serveOps answers op streams on the Worker end of a session: for each it
// reads the op.request and writes whatever handle returns. handle returns the
// frames to send; a nil slice closes the stream without an answer.
func serveOps(t *testing.T, sess *transport.Session, handle func(req OpRequest) []any) {
	t.Helper()
	go func() {
		for {
			st, err := sess.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				body, err := st.ReadFrame()
				if err != nil {
					return
				}
				var req OpRequest
				if err := json.Unmarshal(body, &req); err != nil {
					st.Reset()
					return
				}
				for _, f := range handle(req) {
					if err := st.WriteFrame(f); err != nil {
						return
					}
				}
				_ = st.Close()
			}()
		}
	}()
}

// TestCancelEndsAReadOnTheRealTransportAtOnce runs the Router on a real
// transport session against a Worker that takes the request and stays
// silent: cancelling the context must end the call without waiting for the
// transport's own stream close timeout.
func TestCancelEndsAReadOnTheRealTransportAtOnce(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, idAlice, "pi-1")
	res, err := h.dial(h.workerConfig(key, ops.OpSessionsList))
	if err != nil {
		t.Fatal(err)
	}
	decodeWelcome(t, res)
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	got := make(chan struct{}, 4)
	serveOps(t, sess, func(OpRequest) []any {
		got <- struct{}{}
		time.Sleep(20 * time.Second) // alive and silent
		return nil
	})
	az := authz.New(fakeUsers{idAlice: {Role: authz.RoleUser, Active: true}}, h.svc.OwnerLookup())
	rt, err := NewRouter(RouterConfig{Backend: RegistryBackend(h.svc.Registry()), Directory: StoreDirectory{Store: h.st}, Authz: az, ResultTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	req := CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: w.ID, Args: ops.SessionsListArgs{}}

	// ResultTimeout ends a read.
	start := time.Now()
	if _, err := rt.Call(context.Background(), req); code(err) != proto.CodeWorkerOffline {
		t.Fatalf("timeout: err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the timeout took %v", d)
	}

	// Cancelling the context ends a read that has no time limit of its own.
	<-got
	rt.timeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := rt.Call(ctx, req)
		done <- err
	}()
	<-got
	cancel()
	select {
	case err := <-done:
		if code(err) != proto.CodeWorkerOffline {
			t.Errorf("cancel: err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled call stayed blocked on a silent Worker")
	}
}

func TestInProcessHubRoutesToATestWorker(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, idAlice, "pi-1")
	other := newKey(t)
	foreign := h.enrol(other, idBob, "box")

	res, err := h.dial(h.workerConfig(key, ops.OpSessionsList, ops.OpSendText, ops.OpPipeWait))
	if err != nil {
		t.Fatal(err)
	}
	decodeWelcome(t, res)
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })

	var mu sync.Mutex
	var seen []OpRequest
	serveOps(t, sess, func(req OpRequest) []any {
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		switch req.Op {
		case ops.OpSessionsList:
			return []any{
				map[string]any{"type": proto.TypeOpProgress, "data": map[string]any{"note": "scanning"}},
				map[string]any{"type": proto.TypeOpResult, "result": sessionsResult()},
			}
		case ops.OpSendText:
			return []any{map[string]any{"type": proto.TypeOpResult, "result": ops.WriteResult{Outcome: ops.OutcomeDelivered}}}
		case ops.OpPipeWait:
			turn := func(text string) any {
				return map[string]any{"type": proto.TypeOpProgress, "data": map[string]any{"turn_id": ops.NewUUIDv7(), "session": "minion-1", "direction": "received", "text": text, "at": "2026-11-09T12:00:00.000Z"}}
			}
			return []any{turn("a"), turn("b"), map[string]any{"type": proto.TypeOpResult}}
		}
		return nil // closes the stream without an answer
	})

	az := authz.New(fakeUsers{
		idAlice: {Role: authz.RoleUser, Active: true},
		idBob:   {Role: authz.RoleUser, Active: true},
	}, h.svc.OwnerLookup())
	rt, err := NewRouter(RouterConfig{
		Backend: RegistryBackend(h.svc.Registry()), Directory: StoreDirectory{Store: h.st}, Authz: az,
		OwnerName: func(_ context.Context, id string) string { return ownerNames[id] },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// The human target resolves in Alice's space, then sessions_list runs.
	target, err := rt.Resolve(ctx, routeAlice(), "pi-1:minion-1", false)
	if err != nil || target.WorkerID != w.ID {
		t.Fatalf("resolve: %+v, %v", target, err)
	}
	out, err := rt.Call(ctx, CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: target.WorkerID, Args: ops.SessionsListArgs{}})
	if err != nil {
		t.Fatal(err)
	}
	if list := out.(ops.SessionsListResult); len(list.Sessions) != 1 || list.Sessions[0].Name != "minion-1" {
		t.Errorf("sessions_list = %+v", list)
	}

	// send_text carries the request id, the target and Alice's effective actions.
	id := ops.NewUUIDv7()
	out, err = rt.Call(ctx, CallRequest{
		Principal: routeAlice(), Op: ops.OpSendText, WorkerID: target.WorkerID, Session: target.SessionName,
		Args: ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: id}, Target: target, Text: "go"},
	})
	if err != nil || out.(ops.WriteResult).Outcome != ops.OutcomeDelivered {
		t.Fatalf("send_text: %#v, %v", out, err)
	}
	mu.Lock()
	last := seen[len(seen)-1]
	mu.Unlock()
	if last.Op != ops.OpSendText || last.RequestID != id || !reflect.DeepEqual(last.Actions, ops.ActionSet{ops.ActionMessage, ops.ActionOperate, ops.ActionRead}) {
		t.Errorf("Worker saw %+v", last)
	}

	// A streaming op relays its elements.
	var texts []string
	err = rt.Stream(ctx, CallRequest{
		Principal: routeAlice(), Op: ops.OpPipeWait, WorkerID: target.WorkerID, Session: target.SessionName,
		Args: ops.PipeWaitArgs{Target: target},
	}, func(v any) error { texts = append(texts, v.(ops.PipeTurn).Text); return nil })
	if err != nil || !reflect.DeepEqual(texts, []string{"a", "b"}) {
		t.Errorf("pipe_wait = %v, %v", texts, err)
	}

	// An op the Worker did not announce is unsupported_op, without a stream.
	before := len(seen)
	_, err = rt.Call(ctx, CallRequest{Principal: routeAlice(), Op: ops.OpSessionSnapshot, WorkerID: w.ID, Session: "s", Args: ops.SessionSnapshotArgs{Target: ops.Target{WorkerID: w.ID, SessionName: "s"}}})
	if code(err) != proto.CodeUnsupportedOp {
		t.Errorf("unannounced op: %v", err)
	}
	// A foreign Worker is not reachable for Alice.
	_, err = rt.Call(ctx, CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: foreign.ID, Args: ops.SessionsListArgs{}})
	if code(err) != proto.CodeNotFound {
		t.Errorf("foreign Worker: %v", err)
	}
	mu.Lock()
	if len(seen) != before {
		t.Errorf("the Worker saw %d extra requests", len(seen)-before)
	}
	mu.Unlock()

	// workers_list data from the real store and registry.
	list, err := rt.ListWorkers(ctx, routeAlice(), false)
	if err != nil || len(list) != 1 || list[0].ID != w.ID || !list[0].Online || list[0].Owner != "alice" {
		t.Errorf("ListWorkers = %+v, %v", list, err)
	}

	// The Worker's connection ends: the Worker is offline.
	_ = sess.Close()
	eventually(t, "the Worker to go offline", func() bool { return !h.svc.Registry().IsOnline(w.ID) })
	_, err = rt.Call(ctx, CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: w.ID, Args: ops.SessionsListArgs{}})
	if code(err) != proto.CodeWorkerOffline {
		t.Errorf("offline Worker: %v", err)
	}
}

// TestInProcessWriteLostWhileWaitingIsUnknown ends the Worker's connection
// while a write waits for its answer.
func TestInProcessWriteLostWhileWaitingIsUnknown(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, idAlice, "pi-1")
	res, err := h.dial(h.workerConfig(key, ops.OpSendText))
	if err != nil {
		t.Fatal(err)
	}
	decodeWelcome(t, res)
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })

	got := make(chan OpRequest, 4)
	go func() {
		for {
			st, err := sess.Accept(context.Background())
			if err != nil {
				return
			}
			body, err := st.ReadFrame()
			if err != nil {
				return
			}
			var req OpRequest
			_ = json.Unmarshal(body, &req)
			got <- req
			_ = sess.Close() // the Worker dies after taking the request, before answering
		}
	}()

	az := authz.New(fakeUsers{idAlice: {Role: authz.RoleUser, Active: true}}, h.svc.OwnerLookup())
	rt, err := NewRouter(RouterConfig{Backend: RegistryBackend(h.svc.Registry()), Directory: StoreDirectory{Store: h.st}, Authz: az})
	if err != nil {
		t.Fatal(err)
	}
	target := ops.Target{WorkerID: w.ID, SessionName: "minion-1"}
	id := ops.NewUUIDv7()
	out, err := rt.Call(context.Background(), CallRequest{
		Principal: routeAlice(), Op: ops.OpSendText, WorkerID: w.ID, Session: "minion-1",
		Args: ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: id}, Target: target, Text: "x"},
	})
	requireUnknown(t, out, err)
	select {
	case req := <-got:
		if req.RequestID != id {
			t.Errorf("the Worker saw request %s", req.RequestID)
		}
	default:
		t.Error("the Worker never saw the request")
	}
	if len(got) != 0 {
		t.Error("the write was sent more than once")
	}
}
