package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// --- fakes (names are prefixed wo so they never clash with other test files) ---

// woRouter is a WorkerRouter that records the requests it gets.
type woRouter struct {
	mu       sync.Mutex
	calls    []workers.CallRequest
	callRes  any
	callErr  error
	streamFn func(emit func(any) error) error
}

func (r *woRouter) Call(_ context.Context, req workers.CallRequest) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, req)
	return r.callRes, r.callErr
}

func (r *woRouter) Stream(_ context.Context, req workers.CallRequest, emit func(any) error) error {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	fn := r.streamFn
	r.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(emit)
}

func (r *woRouter) last(t *testing.T) workers.CallRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		t.Fatal("the router was not called")
	}
	return r.calls[len(r.calls)-1]
}

type woUsers map[string]authz.UserState

func (f woUsers) User(_ context.Context, id string) (authz.UserState, error) {
	u, ok := f[id]
	if !ok {
		return authz.UserState{}, authz.ErrNotFound
	}
	return u, nil
}

type woOwners map[string]string

func (f woOwners) WorkerOwner(_ context.Context, id string) (string, error) {
	o, ok := f[id]
	if !ok {
		return "", authz.ErrNotFound
	}
	return o, nil
}

type woAuthn struct{ id Identity }

func (a woAuthn) Authenticate(*http.Request) (Identity, error) { return a.id, nil }

func woIdentity(p authz.Principal) Identity {
	return Identity{Principal: p, Actor: p.UserID, Via: ViaSession, IP: netip.MustParseAddr("192.0.2.9")}
}

// woServer is the REST dispatcher with the real authorizer and the Worker op
// handlers registered on rt.
type woServer struct {
	mux *http.ServeMux
	reg *Registry
}

func newWOServer(t *testing.T, cat *ops.Registry, p authz.Principal, owners woOwners, register func(*Registry)) *woServer {
	t.Helper()
	az := authz.New(woUsers{
		"alice": {Role: authz.RoleUser, Active: true},
		"bob":   {Role: authz.RoleUser, Active: true},
		"root":  {Role: authz.RoleAdmin, Active: true},
	}, owners)
	reg := NewRegistry(cat)
	register(reg)
	mux := http.NewServeMux()
	d := &Dispatcher{Catalog: cat, Handlers: reg, Auth: woAuthn{woIdentity(p)}, Authz: az, Log: slog.New(slog.DiscardHandler)}
	if _, err := NewRouter(d, mux); err != nil {
		t.Fatal(err)
	}
	return &woServer{mux: mux, reg: reg}
}

func (s *woServer) do(method, path, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, httptest.NewRequest(method, path, rd))
	return w
}

func woErrCode(t *testing.T, w *httptest.ResponseRecorder) proto.Code {
	t.Helper()
	var b struct {
		Error struct {
			Code proto.Code `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.Error.Code == "" {
		t.Fatalf("not an error envelope (%d): %s", w.Code, w.Body)
	}
	if w.Code != proto.HTTPStatus(b.Error.Code) {
		t.Errorf("status %d does not match %s", w.Code, b.Error.Code)
	}
	return b.Error.Code
}

func mustRegister(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// --- registration ---

func TestRegisterWorkerOpsRegistersTheElevenCoreOps(t *testing.T) {
	reg := NewRegistry(ops.Default)
	mustRegister(t, RegisterWorkerOps(reg, &woRouter{}))
	names := reg.Names()
	for _, n := range ops.CoreWorkerOpNames {
		if !slices.Contains(names, n) {
			t.Errorf("%s has no handler", n)
		}
	}
	for _, n := range names {
		if !slices.Contains(ops.CoreWorkerOpNames, n) {
			t.Errorf("unexpected handler %s", n)
		}
	}
	if len(ops.CoreWorkerOpNames) != 11 {
		t.Errorf("%d core ops", len(ops.CoreWorkerOpNames))
	}
	// A second registration is refused by the registry, not silently merged.
	if err := RegisterWorkerOps(reg, &woRouter{}); err == nil {
		t.Error("registering the ops twice must fail")
	}
}

func TestRegisterWorkerOpRefusesWhatIsNotAWorkerOp(t *testing.T) {
	reg := NewRegistry(ops.Default)
	if err := RegisterWorkerOp(reg, "no_such_op", &woRouter{}); err == nil {
		t.Error("an op outside the catalog was accepted")
	}
	if err := RegisterWorkerOp(reg, ops.OpWorkersList, &woRouter{}); err == nil {
		t.Error("a Hub op was accepted as a Worker op")
	}
}

// --- routing through the dispatcher ---

func TestWriteOpIsRoutedWithTheTargetAndThePrincipal(t *testing.T) {
	rt := &woRouter{callRes: ops.WriteResult{Outcome: ops.OutcomeDelivered}}
	wid := ops.NewUUIDv7()
	alice := authz.NewSessionPrincipal("alice")
	s := newWOServer(t, ops.Default, alice, woOwners{wid: "alice"}, func(r *Registry) { mustRegister(t, RegisterWorkerOps(r, rt)) })

	rid := ops.NewUUIDv7()
	w := s.do("POST", "/api/v1/ops/send_text", `{"request_id":"`+rid+`","target":{"worker_id":"`+wid+`","session_name":"minion-3","pane":"0.1"},"text":"hi"}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Errorf("body = %s (%v)", w.Body, err)
	}
	req := rt.last(t)
	if req.Op != ops.OpSendText || req.WorkerID != wid || req.Session != "minion-3" || req.Principal.UserID != "alice" {
		t.Errorf("request = %+v", req)
	}
	args, ok := req.Args.(*ops.SendTextArgs)
	if !ok || args.RequestID != rid || args.Text != "hi" || args.Target.Pane != "0.1" {
		t.Errorf("args = %#v", req.Args)
	}
}

func TestEveryCoreOpWithATargetReachesTheRouter(t *testing.T) {
	wid := ops.NewUUIDv7()
	tg := `"target":{"worker_id":"` + wid + `","session_name":"minion-1"}`
	rid := `"request_id":"` + ops.NewUUIDv7() + `"`
	bodies := map[string]string{
		ops.OpSessionSnapshot: `{` + tg + `}`,
		ops.OpSessionCapture:  `{` + tg + `}`,
		ops.OpSendText:        `{` + rid + `,` + tg + `,"text":"x"}`,
		ops.OpSendKeys:        `{` + rid + `,` + tg + `,"keys":["Enter"]}`,
		ops.OpSetTask:         `{` + rid + `,` + tg + `,"task":"t"}`,
		ops.OpAnswer:          `{` + rid + `,` + tg + `,"choice":"1","expect":{"pane_id":"%1","pid":1001,"command":"claude","session_created":1794308400,"dialog_digest":"` + strings.Repeat("a", 64) + `"}}`,
		ops.OpPipeSend:        `{` + rid + `,` + tg + `,"text":"x"}`,
		ops.OpPipeRead:        `{` + tg + `}`,
	}
	for name, body := range bodies {
		rt := &woRouter{callRes: ops.WriteResult{Outcome: ops.OutcomeDelivered}}
		s := newWOServer(t, ops.Default, authz.NewSessionPrincipal("alice"), woOwners{wid: "alice"}, func(r *Registry) { mustRegister(t, RegisterWorkerOps(r, rt)) })
		if w := s.do("POST", "/api/v1/ops/"+name, body); w.Code != 200 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
			continue
		}
		if req := rt.last(t); req.Op != name || req.WorkerID != wid || req.Session != "minion-1" {
			t.Errorf("%s: request = %+v", name, req)
		}
	}
}

func TestRequestLevelWorkerIDRoutesSessionsListAndSessionCreate(t *testing.T) {
	// sessions_list and session_create have no Worker member in their
	// arguments; the dispatcher decides them on the worker_id of the request
	// (query string of a GET, or the JSON body of a POST) and the handler
	// routes to that decision. A POST takes its arguments from the body only,
	// so a worker_id in its query string is not read (S3.9).
	wid := ops.NewUUIDv7()
	rid := ops.NewUUIDv7()
	for _, tc := range []struct {
		name, method, path, body, op string
	}{
		{"sessions_list query", "GET", "/api/v1/ops/sessions_list?worker_id=" + wid, "", ops.OpSessionsList},
		{"sessions_list body", "POST", "/api/v1/ops/sessions_list", `{"worker_id":"` + wid + `"}`, ops.OpSessionsList},
		{"session_create body", "POST", "/api/v1/ops/session_create", `{"worker_id":"` + wid + `","request_id":"` + rid + `","name":"build"}`, ops.OpSessionCreate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &woRouter{callRes: ops.WriteResult{Outcome: ops.OutcomeDelivered}}
			if tc.op == ops.OpSessionsList {
				rt.callRes = ops.SessionsListResult{}
			}
			s := newWOServer(t, ops.Default, authz.NewSessionPrincipal("alice"), woOwners{wid: "alice"}, func(r *Registry) { mustRegister(t, RegisterWorkerOps(r, rt)) })
			if w := s.do(tc.method, tc.path, tc.body); w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if req := rt.last(t); req.Op != tc.op || req.WorkerID != wid || req.Session != "" {
				t.Errorf("request = %+v", req)
			}
		})
	}
}

func TestSessionsListWithoutAWorkerIDStaysInvalid(t *testing.T) {
	rt := &woRouter{}
	s := newWOServer(t, ops.Default, authz.NewSessionPrincipal("alice"), woOwners{}, func(r *Registry) { mustRegister(t, RegisterWorkerOps(r, rt)) })
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/v1/ops/sessions_list", `{}`},
		{"GET", "/api/v1/ops/sessions_list", ""},
		{"POST", "/api/v1/ops/session_create", `{"request_id":"` + ops.NewUUIDv7() + `"}`},
	} {
		w := s.do(tc.method, tc.path, tc.body)
		if woErrCode(t, w) != proto.CodeInvalidArgument || !strings.Contains(w.Body.String(), "worker_id") {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
	if len(rt.calls) != 0 {
		t.Error("the router was called without a Worker")
	}
}

func TestWorkerOpHandlerRefusesAResourceThatIsNoWorker(t *testing.T) {
	// A decision on anything but a Worker or a session cannot be routed.
	rt := &woRouter{}
	r := NewRegistry(ops.Default)
	mustRegister(t, RegisterWorkerOps(r, rt))
	e, ok := r.lookup(ops.OpSessionsList)
	if !ok {
		t.Fatal("sessions_list has no handler")
	}
	_, err := e.handler(context.Background(), &Call{Args: &ops.SessionsListArgs{}, Resource: authz.UserResource{ID: "alice"}})
	if err == nil || len(rt.calls) != 0 {
		t.Errorf("err = %v, calls = %d", err, len(rt.calls))
	}
}

func TestAWorkerLevelOpRoutesByWorkerID(t *testing.T) {
	// The share ops of Story 4.15 have the worker_id shape: no session.
	cat := testCatalog(t)
	rt := &woRouter{callRes: shareResult{Mode: "all"}}
	wid := ops.NewUUIDv7()
	for _, tc := range []struct {
		name    string
		p       authz.Principal
		owners  woOwners
		wantErr proto.Code
	}{
		{"owner", authz.NewSessionPrincipal("alice"), woOwners{wid: "alice"}, ""},
		{"admin", authz.NewSessionPrincipal("root"), woOwners{wid: "alice"}, ""},
		{"foreign owner", authz.NewSessionPrincipal("bob"), woOwners{wid: "alice"}, proto.CodeNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt.calls = nil
			s := newWOServer(t, cat, tc.p, tc.owners, func(r *Registry) { mustRegister(t, RegisterWorkerOp(r, opShareGet, rt)) })
			w := s.do("POST", "/api/v1/ops/"+opShareGet, `{"worker_id":"`+wid+`"}`)
			if tc.wantErr != "" {
				if woErrCode(t, w) != tc.wantErr || len(rt.calls) != 0 {
					t.Errorf("%d %s (calls %d)", w.Code, w.Body, len(rt.calls))
				}
				return
			}
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"mode":"all"`) {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if req := rt.last(t); req.WorkerID != wid || req.Session != "" || req.Op != opShareGet {
				t.Errorf("request = %+v", req)
			}
		})
	}
}

func TestDispatcherDeniesBeforeTheRouter(t *testing.T) {
	rt := &woRouter{}
	wid := ops.NewUUIDv7()
	s := newWOServer(t, ops.Default, authz.NewSessionPrincipal("bob"), woOwners{wid: "alice"}, func(r *Registry) { mustRegister(t, RegisterWorkerOps(r, rt)) })
	w := s.do("POST", "/api/v1/ops/session_snapshot", `{"target":{"worker_id":"`+wid+`","session_name":"s"}}`)
	if woErrCode(t, w) != proto.CodeNotFound || len(rt.calls) != 0 {
		t.Errorf("%d %s (calls %d)", w.Code, w.Body, len(rt.calls))
	}
}

func TestRouterErrorsKeepTheirCodes(t *testing.T) {
	wid := ops.NewUUIDv7()
	body := `{"target":{"worker_id":"` + wid + `","session_name":"s"}}`
	for _, code := range []proto.Code{proto.CodeWorkerOffline, proto.CodeUnsupportedOp, proto.CodePreviewOnly, proto.CodeConflict} {
		rt := &woRouter{callErr: proto.NewError(code, "no")}
		s := newWOServer(t, ops.Default, authz.NewSessionPrincipal("alice"), woOwners{wid: "alice"}, func(r *Registry) { mustRegister(t, RegisterWorkerOps(r, rt)) })
		if got := woErrCode(t, s.do("POST", "/api/v1/ops/session_snapshot", body)); got != code {
			t.Errorf("code = %s, want %s", got, code)
		}
	}
}

func TestStreamOpEmitsTheRoutersElements(t *testing.T) {
	wid := ops.NewUUIDv7()
	rt := &woRouter{streamFn: func(emit func(any) error) error {
		for _, text := range []string{"a", "b"} {
			if err := emit(ops.PipeTurn{TurnID: ops.NewUUIDv7(), Session: "s", Direction: ops.PipeReceived, Text: text, At: ops.Timestamp(time.UnixMilli(1_700_000_000_000))}); err != nil {
				return err
			}
		}
		return nil
	}}
	s := newWOServer(t, ops.Default, authz.NewSessionPrincipal("alice"), woOwners{wid: "alice"}, func(r *Registry) { mustRegister(t, RegisterWorkerOps(r, rt)) })
	w := s.do("POST", "/api/v1/ops/pipe_wait", `{"target":{"worker_id":"`+wid+`","session_name":"s"}}`)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("%d %s %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	var texts []string
	sc := bufio.NewScanner(w.Body)
	for sc.Scan() {
		var turn ops.PipeTurn
		if err := json.Unmarshal(sc.Bytes(), &turn); err != nil {
			t.Fatal(err)
		}
		texts = append(texts, turn.Text)
	}
	if !slices.Equal(texts, []string{"a", "b"}) {
		t.Errorf("texts = %v", texts)
	}
	if req := rt.last(t); req.Op != ops.OpPipeWait || req.Session != "s" {
		t.Errorf("request = %+v", req)
	}

	// An error before the first element is an enveloped error.
	rt2 := &woRouter{streamFn: func(func(any) error) error { return proto.NewError(proto.CodeWorkerOffline, "gone") }}
	s = newWOServer(t, ops.Default, authz.NewSessionPrincipal("alice"), woOwners{wid: "alice"}, func(r *Registry) { mustRegister(t, RegisterWorkerOps(r, rt2)) })
	if got := woErrCode(t, s.do("POST", "/api/v1/ops/pipe_wait", `{"target":{"worker_id":"`+wid+`","session_name":"s"}}`)); got != proto.CodeWorkerOffline {
		t.Errorf("code = %s", got)
	}
}

// --- workers_list ---

type woLister struct {
	p    authz.Principal
	all  bool
	list []ops.WorkerInfo
	err  error
}

func (l *woLister) ListWorkers(_ context.Context, p authz.Principal, all bool) ([]ops.WorkerInfo, error) {
	l.p, l.all = p, all
	return l.list, l.err
}

func TestWorkersListHandler(t *testing.T) {
	l := &woLister{list: []ops.WorkerInfo{{ID: "w1", Name: "pi-1", OwnerID: "alice", Owner: "alice", Kind: ops.WorkerKindPrivate, Online: true}}}
	s := newWOServer(t, ops.Default, authz.NewSessionPrincipal("root"), woOwners{}, func(r *Registry) { mustRegister(t, RegisterWorkersList(r, l)) })

	w := s.do("GET", "/api/v1/ops/workers_list", "")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if l.all || l.p.UserID != "root" {
		t.Errorf("default must be mine: all=%v principal=%+v", l.all, l.p)
	}
	var res ops.WorkersListResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || len(res.Workers) != 1 || !res.Workers[0].Online || res.Workers[0].Name != "pi-1" {
		t.Errorf("body = %s (%v)", w.Body, err)
	}

	if w := s.do("GET", "/api/v1/ops/workers_list?all=true", ""); w.Code != 200 || !l.all {
		t.Errorf("all=true: %d, all=%v", w.Code, l.all)
	}
	if w := s.do("POST", "/api/v1/ops/workers_list", `{"all":true}`); w.Code != 200 || !l.all {
		t.Errorf("POST all: %d, all=%v", w.Code, l.all)
	}

	// An empty list is [] and not null; a failing lister is an error.
	l.list = nil
	if w := s.do("POST", "/api/v1/ops/workers_list", `{}`); !strings.Contains(w.Body.String(), `"workers":[]`) {
		t.Errorf("empty list = %s", w.Body)
	}
	l.err = proto.NewError(proto.CodeForbidden, "no")
	if got := woErrCode(t, s.do("POST", "/api/v1/ops/workers_list", `{}`)); got != proto.CodeForbidden {
		t.Errorf("code = %s", got)
	}
}

// --- the whole path: REST, Router, registry, a test Worker over a real session ---

type woDir []workers.WorkerRecord

func (d woDir) Workers(context.Context) ([]workers.WorkerRecord, error) { return d, nil }

func TestRESTToATestWorkerEndToEnd(t *testing.T) {
	wAlice := workers.WorkerRecord{ID: ops.NewUUIDv7(), Name: "pi-1", OwnerID: "alice", Kind: "private"}
	wBob := workers.WorkerRecord{ID: ops.NewUUIDv7(), Name: "box", OwnerID: "bob", Kind: "infrastructure"}
	dir := woDir{wAlice, wBob}

	// A real Hub session and Worker session over an in-memory connection.
	a, b := net.Pipe()
	hubSess, err := transport.NewSession(a, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	workerSess, err := transport.NewSession(b, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hubSess.Close(); _ = workerSess.Close() })

	reg := workers.NewRegistry()
	reg.Add(workers.Online{
		WorkerID: wAlice.ID, Name: wAlice.Name, OwnerID: "alice", Kind: "private", Version: proto.Version{Major: 1},
		Ops: []string{ops.OpSessionSnapshot, ops.OpSendText, ops.OpSessionsList},
	}, hubSess)

	var mu sync.Mutex
	var seen []workers.OpRequest
	go func() {
		for {
			st, err := workerSess.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				body, err := st.ReadFrame()
				if err != nil {
					return
				}
				var req workers.OpRequest
				_ = json.Unmarshal(body, &req)
				mu.Lock()
				seen = append(seen, req)
				mu.Unlock()
				switch req.Op {
				case ops.OpSendText:
					_ = st.WriteFrame(map[string]any{"type": proto.TypeOpResult, "result": ops.WriteResult{Outcome: ops.OutcomeDelivered, Pane: &ops.PaneInfo{Session: "minion-3", ID: "%1"}}})
				case ops.OpSessionsList:
					_ = st.WriteFrame(map[string]any{"type": proto.TypeOpResult, "result": ops.SessionsListResult{Sessions: []ops.SessionInfo{{Name: "minion-3", Windows: 1}}}})
				case ops.OpSessionSnapshot:
					_ = st.WriteFrame(proto.NewError(proto.CodeNotShared, "not shared").Frame())
				}
				_ = st.Close()
			}()
		}
	}()

	az := authz.New(woUsers{
		"alice": {Role: authz.RoleUser, Active: true},
		"bob":   {Role: authz.RoleUser, Active: true},
		"root":  {Role: authz.RoleAdmin, Active: true},
	}, woOwners{wAlice.ID: "alice", wBob.ID: "bob"})
	rt, err := workers.NewRouter(workers.RouterConfig{Backend: workers.RegistryBackend(reg), Directory: dir, Authz: az, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	serve := func(p authz.Principal) *woServer {
		return newWOServer(t, ops.Default, p, woOwners{wAlice.ID: "alice", wBob.ID: "bob"}, func(r *Registry) {
			mustRegister(t, RegisterWorkerOps(r, rt))
			mustRegister(t, RegisterWorkersList(r, rt))
		})
	}
	alice := serve(authz.NewSessionPrincipal("alice"))

	// workers_list: Alice sees her own Worker, online, with its ops.
	w := alice.do("GET", "/api/v1/ops/workers_list?all=true", "")
	var list ops.WorkersListResult
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(list.Workers) != 1 || list.Workers[0].ID != wAlice.ID || !list.Workers[0].Online || !slices.Contains(list.Workers[0].Ops, ops.OpSendText) {
		t.Errorf("workers = %+v", list.Workers)
	}
	admin := serve(authz.NewSessionPrincipal("root"))
	var all, mine ops.WorkersListResult
	_ = json.Unmarshal(admin.do("GET", "/api/v1/ops/workers_list?all=true", "").Body.Bytes(), &all)
	_ = json.Unmarshal(admin.do("GET", "/api/v1/ops/workers_list", "").Body.Bytes(), &mine)
	if len(all.Workers) != 2 || len(mine.Workers) != 0 {
		t.Errorf("admin: all=%d mine=%d (want 2 and 0)", len(all.Workers), len(mine.Workers))
	}

	// sessions_list is routed by the worker_id of the request (query and body).
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/v1/ops/sessions_list?worker_id=" + wAlice.ID, ""},
		{"POST", "/api/v1/ops/sessions_list", `{"worker_id":"` + wAlice.ID + `"}`},
	} {
		w = alice.do(tc.method, tc.path, tc.body)
		var sl ops.SessionsListResult
		if err := json.Unmarshal(w.Body.Bytes(), &sl); err != nil || w.Code != 200 || len(sl.Sessions) != 1 || sl.Sessions[0].Name != "minion-3" {
			t.Fatalf("sessions_list %s: %d %s", tc.method, w.Code, w.Body)
		}
	}
	mu.Lock()
	sreq := seen[len(seen)-1]
	mu.Unlock()
	if sreq.Op != ops.OpSessionsList || !slices.Contains(sreq.Actions, ops.ActionRead) {
		t.Errorf("Worker saw %+v for sessions_list", sreq)
	}
	if w := alice.do("GET", "/api/v1/ops/sessions_list?worker_id="+wBob.ID, ""); woErrCode(t, w) != proto.CodeNotFound {
		t.Errorf("sessions_list on a foreign Worker: %d %s", w.Code, w.Body)
	}

	// send_text reaches the Worker with the caller's effective actions.
	rid := ops.NewUUIDv7()
	w = alice.do("POST", "/api/v1/ops/send_text", `{"request_id":"`+rid+`","target":{"worker_id":"`+wAlice.ID+`","session_name":"minion-3"},"text":"go"}`)
	var wr ops.WriteResult
	if err := json.Unmarshal(w.Body.Bytes(), &wr); err != nil || w.Code != 200 || wr.Outcome != ops.OutcomeDelivered || wr.Pane == nil || wr.Pane.Session != "minion-3" {
		t.Fatalf("send_text: %d %s", w.Code, w.Body)
	}
	mu.Lock()
	req := seen[len(seen)-1]
	mu.Unlock()
	if req.Op != "send_text" || req.RequestID != rid || !slices.Equal(req.Actions, ops.ActionSet{ops.ActionMessage, ops.ActionOperate, ops.ActionRead}) {
		t.Errorf("Worker saw %+v", req)
	}

	// A Worker error frame comes back as the same API code.
	w = alice.do("POST", "/api/v1/ops/session_snapshot", `{"target":{"worker_id":"`+wAlice.ID+`","session_name":"minion-3"}}`)
	if got := woErrCode(t, w); got != proto.CodeNotShared {
		t.Errorf("snapshot code = %s", got)
	}

	// An op the Worker did not announce, a foreign Worker and an offline Worker
	// never reach it.
	before := func() int { mu.Lock(); defer mu.Unlock(); return len(seen) }
	n := before()
	w = alice.do("POST", "/api/v1/ops/session_capture", `{"target":{"worker_id":"`+wAlice.ID+`","session_name":"s"}}`)
	if got := woErrCode(t, w); got != proto.CodeUnsupportedOp {
		t.Errorf("unannounced op: %s", got)
	}
	w = alice.do("POST", "/api/v1/ops/session_snapshot", `{"target":{"worker_id":"`+wBob.ID+`","session_name":"s"}}`)
	if got := woErrCode(t, w); got != proto.CodeNotFound {
		t.Errorf("foreign Worker: %s", got)
	}
	bob := serve(authz.NewSessionPrincipal("bob"))
	w = bob.do("POST", "/api/v1/ops/session_snapshot", `{"target":{"worker_id":"`+wBob.ID+`","session_name":"s"}}`)
	if got := woErrCode(t, w); got != proto.CodeWorkerOffline {
		t.Errorf("offline Worker: %s", got)
	}
	if before() != n {
		t.Errorf("the Worker saw %d requests it should not have seen", before()-n)
	}

	// The connection ends: the Worker is offline.
	_ = workerSess.Close()
	deadline := time.Now().Add(5 * time.Second)
	for reg.IsOnline(wAlice.ID) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	w = alice.do("POST", "/api/v1/ops/session_snapshot", `{"target":{"worker_id":"`+wAlice.ID+`","session_name":"s"}}`)
	if got := woErrCode(t, w); got != proto.CodeWorkerOffline {
		t.Errorf("after disconnect: %s", got)
	}
}
