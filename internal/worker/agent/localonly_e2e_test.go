//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// expectedManagementOps is the independent, hand-maintained list of the
// local-only management ops (tray tickets v3, W0). It is deliberately not read
// from localOps: a new local op must be added here as well, and then it is
// checked against a fully started Worker below. Ops are added by the ticket
// that implements them.
var expectedManagementOps = []string{
	"worker_status",
	"worker_stop",
	"worker_verify_hub",
	"worker_pause",
	"worker_resume",
	"worker_config_apply",
	"worker_share_list",
	"worker_share_apply",
	"worker_lifecycle",
	"worker_pair_cancel",
}

// helloDialer is a pairDialer that records the ops and features of every
// hello the Worker sends.
type helloDialer struct {
	pairDialer
	mu       sync.Mutex
	ops      [][]string
	features [][]string
}

func (d *helloDialer) Connect(ctx context.Context, p ConnectParams) (*Attempt, error) {
	d.mu.Lock()
	d.ops = append(d.ops, slices.Clone(p.Ops))
	d.features = append(d.features, slices.Clone(p.Features))
	d.mu.Unlock()
	return d.pairDialer.Connect(ctx, p)
}

func (d *helloDialer) hellos() (opsSeen, features [][]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.ops), slices.Clone(d.features)
}

func TestLocalOpsMapMatchesExpected(t *testing.T) {
	got := slices.Sorted(maps.Keys(localOps))
	want := slices.Clone(expectedManagementOps)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("local-only ops = %v, want the independent list %v", got, want)
	}
}

// Against one fully started Worker (core, pipe, connector, op server, Hub pipe
// and local socket, with an in-process Hub): every management op answers on
// the owner's socket, is refused as unsupported_op through the authenticated
// Hub op stream without any effect, and is never announced in the hello; a
// normal remote op (sessions_list) succeeds through the same Hub link.
// fullWorker is a fully started Worker (core, pipe, connector, op server,
// Hub pipe and local socket) whose Hub links come from a pairDialer.
type fullWorker struct {
	rt     *Runtime
	dir    string
	host   *anchorHost
	dialer *helloDialer
	client *client.LocalBackend
}

func startFullWorker(t *testing.T) *fullWorker {
	t.Helper()
	dir, _ := enrolledDir(t)
	host, cls := newAnchorHost(t)
	core := coreDeps{
		openStore:  func(ctx context.Context, d string) (*store.Store, error) { return store.OpenContext(ctx, d) },
		newHost:    func(string) (term.SessionHost, error) { return host, nil },
		classifier: cls,
	}
	dialer := &helloDialer{pairDialer: pairDialer{hubs: make(chan *transport.Session, 8)}}
	sock := filepath.Join(shortRunDir(t), "minion", localapi.SocketName)
	lad := defaultLocalAPIDeps()
	lad.socketPath = func() (string, error) { return sock, nil }
	r := NewRegistry()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(RegisterLifecycle(r))
	must(r.Register(ModuleCore, OrderCore, core.start))
	must(r.Register(ModulePipe, OrderPipe, pipeDeps{poll: 10 * time.Millisecond, settle: 1}.start))
	must(r.Register(ModuleConnector, OrderConnector, connectorDeps{dialer: dialer}.start))
	must(r.Register(ModuleOpServer, OrderOpServer, opServerDeps{}.start))
	must(r.Register(ModuleHubPipe, OrderHubPipe, hubPipeDeps{}.start))
	must(r.Register(ModuleLocalAPI, OrderLocalAPI, lad.start))
	env := map[string]string{"MINION_SESSION_POLL_INTERVAL": "100ms", "MINION_SERVER": "https://hub.example"}
	rt := NewRuntime(r, Options{StopTimeout: 5 * time.Second})
	must(rt.Start(context.Background(), Settings{
		WorkerCore: config.WorkerCore{StateDir: dir, TmuxSocket: filepath.Join(dir, "tmux.sock"), Name: "w", ShareExisting: "all"},
		Resolver:   &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }},
		Logger:     slog.New(slog.NewTextHandler(&logBuf{}, nil)),
	}))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	return &fullWorker{rt: rt, dir: dir, host: host, dialer: dialer, client: client.Local(client.WithSocketPath(sock))}
}

// nextHub waits for the next Hub link of the Worker.
func (w *fullWorker) nextHub(t *testing.T) *transport.Session {
	t.Helper()
	select {
	case hub := <-w.dialer.hubs:
		return hub
	case <-time.After(10 * time.Second):
		t.Fatal("the Worker did not connect to the Hub")
		return nil
	}
}

// hubCall sends one op request through the Hub link and returns the answer.
func hubCall(t *testing.T, ctx context.Context, hub *transport.Session, op string, args any) string {
	t.Helper()
	st, err := hub.OpenOp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Reset()
	if err := st.WriteFrame(opRequest(op, []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage, ops.ActionManage}, args)); err != nil {
		t.Fatal(err)
	}
	body, ok := readHubFrame(t, st, 5*time.Second)
	if !ok {
		t.Fatalf("%s: no answer from the Worker", op)
	}
	return string(body)
}

func TestLocalOnlyOpsAgainstFullWorker(t *testing.T) {
	w := startFullWorker(t)
	rt, dialer, c := w.rt, w.dialer, w.client
	// worker_stop asks the runtime to stop (RequestStop closes stopReq).
	stopped := rt.stopReq
	hub := w.nextHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hubCall := func(op string) string { t.Helper(); return hubCall(t, ctx, hub, op, map[string]any{}) }

	// A normal remote op works through the very same link.
	if body := hubCall(ops.OpSessionsList); !strings.Contains(body, proto.TypeOpResult) || !strings.Contains(body, `"work"`) {
		t.Fatalf("sessions_list through the Hub = %s", body)
	}
	before := statusReply(t, c)

	// Through the authenticated Hub op stream every management op is refused.
	for _, op := range expectedManagementOps {
		body := hubCall(op)
		if !strings.Contains(body, string(proto.CodeUnsupportedOp)) || strings.Contains(body, proto.TypeOpResult) {
			t.Errorf("%s through the Hub = %s, want unsupported_op", op, body)
		}
	}
	// Unchanged state: the Worker still runs (worker_stop did nothing) and
	// its status is the same instance.
	time.Sleep(400 * time.Millisecond)
	select {
	case <-stopped:
		t.Fatal("a Hub request stopped the Worker")
	default:
	}
	if after := statusReply(t, c); after.Instance != before.Instance || after.StateDir != before.StateDir {
		t.Fatalf("status changed: before %+v, after %+v", before, after)
	}

	// Never announced: no hello carries a management op or a local feature.
	helloOps, helloFeatures := dialer.hellos()
	if len(helloOps) == 0 {
		t.Fatal("no hello was recorded")
	}
	for i := range helloOps {
		for _, op := range expectedManagementOps {
			if slices.Contains(helloOps[i], op) || slices.Contains(helloFeatures[i], op) {
				t.Errorf("hello %d announces %s: ops %v, features %v", i, op, helloOps[i], helloFeatures[i])
			}
		}
		for _, f := range localFeatures {
			if slices.Contains(helloFeatures[i], f) {
				t.Errorf("hello %d announces the local feature %s", i, f)
			}
		}
	}

	// On the owner's socket each op answers: a result or a validation error,
	// never unsupported_op. worker_stop goes last, it stops the Worker.
	for _, op := range expectedManagementOps {
		if op == OpLocalStop {
			continue
		}
		_, err := c.Call(ctx, op, map[string]any{})
		var pe *proto.Error
		if err != nil && (!errors.As(err, &pe) || pe.Code == proto.CodeUnsupportedOp) {
			t.Errorf("%s on the owner socket: %v", op, err)
		}
	}
	raw, err := c.Call(ctx, OpLocalStop, map[string]any{})
	if err != nil || !json.Valid(raw) || !strings.Contains(string(raw), "stopping") {
		t.Fatalf("worker_stop on the owner socket = %s, %v", raw, err)
	}
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("worker_stop on the owner socket did not stop the Worker")
	}
}
