package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// pingArgs and the ops below are a private catalog for the dispatcher tests.
type pingArgs struct {
	Message string         `json:"message"`
	Limit   int            `json:"limit,omitempty"`
	Flag    bool           `json:"flag,omitempty"`
	At      *ops.Timestamp `json:"at,omitempty"`
	Tags    []string       `json:"tags,omitempty"`
}

func (a pingArgs) Validate() error {
	if len(a.Message) > 16 {
		return &ops.ArgumentError{Field: "message", Reason: "is longer than 16 bytes"}
	}
	return nil
}

type pingResult struct {
	Echo string        `json:"echo"`
	At   ops.Timestamp `json:"at"`
}

type shareArgs struct {
	WorkerID string `json:"worker_id"`
}

type shareResult struct {
	Mode string `json:"mode"`
}

type shareSetArgs struct {
	ops.WriteArgs
	WorkerID string `json:"worker_id"`
	Mode     string `json:"mode"`
}

type tickArgs struct {
	Count int `json:"count,omitempty"`
}

type tickResult struct {
	N int `json:"n"`
}

const (
	opPing     = "ping_read"
	opPingSet  = "ping_set"
	opShareGet = "probe_share_get"
	opShareSet = "probe_share_set"
	opTick     = "tick_stream"
	opPtrRead  = "probe_ptr_read"
	opSessSet  = "probe_session_set"
)

// sessSetArgs is a write that names a session by bare members.
type sessSetArgs struct {
	ops.WriteArgs
	WorkerID    string `json:"worker_id"`
	SessionName string `json:"session_name"`
}

var (
	manageSet  = ops.MustActionSet(ops.ActionManage)
	operateSet = ops.MustActionSet(ops.ActionOperate)
)

// testCatalog returns a catalog with the test ops plus three real Worker ops.
func testCatalog(t testing.TB) *ops.Registry {
	t.Helper()
	cat := ops.NewRegistry()
	guard := ops.Guard{Name: "probe_guard", Class: ops.GuardKeep, Reads: "probe.state"}
	cat.Register(ops.Op{Name: opPing, Summary: "Echo.", Scope: ops.ScopeHub, Actions: manageSet,
		Args: reflect.TypeFor[pingArgs](), Result: reflect.TypeFor[pingResult]()})
	cat.Register(ops.Op{Name: opPingSet, Summary: "Write.", Scope: ops.ScopeHub, Actions: manageSet, Write: true,
		Guards: []ops.Guard{guard}, Args: reflect.TypeFor[pingSetArgs](), Result: reflect.TypeFor[ops.WriteResult]()})
	cat.Register(ops.Op{Name: opShareGet, Summary: "Share rule.", Scope: ops.ScopeWorker, Actions: operateSet,
		Args: reflect.TypeFor[shareArgs](), Result: reflect.TypeFor[shareResult]()})
	cat.Register(ops.Op{Name: opShareSet, Summary: "Set share rule.", Scope: ops.ScopeWorker, Actions: operateSet, Write: true,
		Guards: []ops.Guard{guard}, Args: reflect.TypeFor[shareSetArgs](), Result: reflect.TypeFor[ops.WriteResult]()})
	cat.Register(ops.Op{Name: opTick, Summary: "Stream.", Scope: ops.ScopeHub, Actions: manageSet, Stream: true,
		Args: reflect.TypeFor[tickArgs](), Result: reflect.TypeFor[tickResult]()})
	cat.Register(ops.Op{Name: opPtrRead, Summary: "Optional target.", Scope: ops.ScopeWorker, Actions: operateSet,
		Args: reflect.TypeFor[optTarget](), Result: reflect.TypeFor[shareResult]()})
	cat.Register(ops.Op{Name: opSessSet, Summary: "Session write.", Scope: ops.ScopeWorker, Actions: operateSet, Write: true,
		Guards: []ops.Guard{guard}, Args: reflect.TypeFor[sessSetArgs](), Result: reflect.TypeFor[ops.WriteResult]()})
	for _, n := range []string{ops.OpSessionSnapshot, ops.OpSendText, ops.OpSessionsList, ops.OpSessionCreate} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("ops.Default lacks %s", n)
		}
		cat.Register(op)
	}
	return cat
}

type pingSetArgs struct {
	ops.WriteArgs
	Message string `json:"message"`
}

// fakeAuthn returns a fixed identity or error.
type fakeAuthn struct {
	mu  sync.Mutex
	id  Identity
	err error
	n   int
}

func (f *fakeAuthn) Authenticate(*http.Request) (Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return f.id, f.err
}

// setErr changes the answer for later calls, also while a stream runs.
func (f *fakeAuthn) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeAuthn) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// decideCall is one recorded authz.Decide call.
type decideCall struct {
	p        authz.Principal
	accepted ops.ActionSet
	res      authz.Resource
}

// fakeDecider records calls and answers from fn (nil allows everything).
type fakeDecider struct {
	mu    sync.Mutex
	calls []decideCall
	fn    func(c decideCall) error
	log   *[]string
}

func (f *fakeDecider) Decide(_ context.Context, p authz.Principal, accepted ops.ActionSet, res authz.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := decideCall{p, accepted, res}
	f.calls = append(f.calls, c)
	if f.log != nil {
		*f.log = append(*f.log, "decide")
	}
	if f.fn != nil {
		return f.fn(c)
	}
	return nil
}

func (f *fakeDecider) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeAudit records events.
type fakeAudit struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

func (f *fakeAudit) Record(_ context.Context, ev audit.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ev.Validate(); err != nil { // the real log refuses such events
		return err
	}
	f.events = append(f.events, ev)
	return f.err
}

func (f *fakeAudit) all() []audit.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]audit.Event(nil), f.events...)
}

var testIdentity = Identity{
	Principal: authz.NewSessionPrincipal("user-1"), Actor: "user-1", Via: ViaSession,
	IP: netip.MustParseAddr("192.0.2.7"),
}

// rig is a dispatcher on a mux with fakes.
type rig struct {
	d      *Dispatcher
	mux    *http.ServeMux
	router *Router
	authn  *fakeAuthn
	decide *fakeDecider
	audit  *fakeAudit
	reg    *Registry
	logs   *strings.Builder
}

func newRig(t *testing.T) *rig {
	t.Helper()
	cat := testCatalog(t)
	r := &rig{
		authn: &fakeAuthn{id: testIdentity}, decide: &fakeDecider{}, audit: &fakeAudit{},
		reg: NewRegistry(cat), mux: http.NewServeMux(), logs: &strings.Builder{},
	}
	r.d = &Dispatcher{Catalog: cat, Handlers: r.reg, Auth: r.authn, Authz: r.decide, Audit: r.audit,
		Log: testLogger(r.logs)}
	var err error
	if r.router, err = NewRouter(r.d, r.mux); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) do(method, path string, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	w := httptest.NewRecorder()
	r.mux.ServeHTTP(w, req)
	return w
}

func mustHandle(t *testing.T, r *Registry, name string, fn Handler, opts ...HandlerOption) {
	t.Helper()
	if err := r.Handle(name, fn, opts...); err != nil {
		t.Fatal(err)
	}
}

var errBoom = errors.New("boom: secret detail /var/lib/minion.db")
