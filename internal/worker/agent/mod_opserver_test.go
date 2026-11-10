package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

func TestOpServerModuleIsRegisteredAfterTheConnector(t *testing.T) {
	names := DefaultRegistry.Names()
	conn, mod := slices.Index(names, ModuleConnector), slices.Index(names, ModuleOpServer)
	if conn < 0 || mod < 0 || mod < conn {
		t.Fatalf("default modules = %v, want the op server after the connector", names)
	}
}

func TestOpServerModuleNeedsTheConnector(t *testing.T) {
	r := NewRegistry()
	core := coreDeps{
		openStore:  func(ctx context.Context, d string) (*store.Store, error) { return store.OpenContext(ctx, d) },
		newHost:    func(string) (term.SessionHost, error) { return newFakeHost(), nil },
		classifier: newFakeClassifier(),
	}
	_ = r.Register(ModuleCore, OrderCore, core.start)
	_ = r.Register(ModuleOpServer, OrderOpServer, opServerDeps{}.start)
	err := NewRuntime(r, Options{}).Start(context.Background(), Settings{WorkerCore: config.WorkerCore{StateDir: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "requires service") {
		t.Fatalf("err = %v", err)
	}
}

// pairDialer hands out real transport sessions over net.Pipe: the Worker end
// to the connector, the Hub end to the test.
type pairDialer struct {
	hubs chan *transport.Session
}

func (d *pairDialer) Connect(_ context.Context, p ConnectParams) (*Attempt, error) {
	a, b := net.Pipe()
	hub, err := transport.NewSession(a, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		return nil, err
	}
	worker, err := transport.NewSession(b, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		return nil, err
	}
	d.hubs <- hub
	return &Attempt{
		Pin: p.Pin, TranscriptHash: randomHash(), Version: proto.Current,
		Reply:   Reply{Type: proto.TypeWelcome, Welcome: okWelcome()},
		Session: worker,
	}, nil
}

// readHubFrame reads one frame from a stream the Hub accepted or opened.
func readHubFrame(t *testing.T, st *transport.Stream, timeout time.Duration) (json.RawMessage, bool) {
	t.Helper()
	_ = st.SetReadDeadline(time.Now().Add(timeout))
	body, err := st.ReadFrame()
	return body, err == nil
}

// The whole module: the connector welcomes a link, the op server answers the
// Hub's op stream through the real executor, the event stream opens with a
// snapshot on every (re)connect, and an unshared session shows up nowhere.
func TestOpServerModuleServesLinksAndReconnects(t *testing.T) {
	dir := t.TempDir()
	hubKey, _ := identity.GenerateKey()
	st0, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	enrolledState(t, hubKey, false)(dir, st0)
	if err := st0.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"open"}}); err != nil {
		t.Fatal(err)
	}
	_ = st0.Close()

	host := newFakeHost()
	cls := newFakeClassifier()
	cls.set("claude", "agent:claude", ops.PaneStateStable)
	for _, n := range []string{"open", "secret"} {
		ref := host.addSession(n, "claude")
		host.setScreen(ref, "content of "+n)
	}
	core := coreDeps{
		openStore:  func(ctx context.Context, d string) (*store.Store, error) { return store.OpenContext(ctx, d) },
		newHost:    func(string) (term.SessionHost, error) { return host, nil },
		classifier: cls,
	}
	dialer := &pairDialer{hubs: make(chan *transport.Session, 4)}
	r := NewRegistry()
	_ = r.Register(ModuleCore, OrderCore, core.start)
	_ = r.Register(ModuleConnector, OrderConnector, connectorDeps{dialer: dialer}.start)
	_ = r.Register(ModuleOpServer, OrderOpServer, opServerDeps{}.start)
	env := map[string]string{"MINION_SERVER": "https://hub.example", "MINION_SESSION_POLL_INTERVAL": "100ms"}
	rt := NewRuntime(r, Options{})
	err = rt.Start(context.Background(), Settings{
		WorkerCore: config.WorkerCore{StateDir: dir, TmuxSocket: filepath.Join(dir, "tmux.sock"), Name: "w"},
		Resolver:   &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }},
		Logger:     slog.New(slog.NewTextHandler(&logBuf{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	if _, ok := Lookup[*OpServer](rt); !ok {
		t.Fatal("no op server service")
	}

	for conn := 1; conn <= 2; conn++ {
		var hub *transport.Session
		select {
		case hub = <-dialer.hubs:
		case <-time.After(10 * time.Second):
			t.Fatalf("connection %d never came", conn)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		// The Worker opens the event stream; its first frame is the snapshot.
		evs, err := hub.Accept(ctx)
		if err != nil || evs.Kind() != transport.KindEvents {
			t.Fatalf("connection %d: accept = %v, %v", conn, evs, err)
		}
		body, ok := readHubFrame(t, evs, 5*time.Second)
		if !ok {
			t.Fatalf("connection %d: no snapshot", conn)
		}
		var snap SnapshotFrame
		if err := json.Unmarshal(body, &snap); err != nil || snap.Type != proto.TypeEventsSnap || snap.Seq != 0 {
			t.Fatalf("connection %d: snapshot = %s", conn, body)
		}
		if len(snap.Sessions) != 1 || snap.Sessions[0].Name != "open" || strings.Contains(string(body), "secret") {
			t.Fatalf("connection %d: snapshot = %s", conn, body)
		}

		// The Hub's op stream is answered through the executor.
		op, err := hub.OpenOp(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := op.WriteFrame(opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil)); err != nil {
			t.Fatal(err)
		}
		body, ok = readHubFrame(t, op, 5*time.Second)
		if !ok || !strings.Contains(string(body), proto.TypeOpResult) || !strings.Contains(string(body), `"open"`) || strings.Contains(string(body), "secret") {
			t.Fatalf("connection %d: sessions_list answer = %s", conn, body)
		}

		// A change in a shared session is published with seq 1.
		cls.set("claude", "agent:claude", ops.PaneStateBusy)
		body, ok = readHubFrame(t, evs, 5*time.Second)
		var ev EventFrame
		if !ok || json.Unmarshal(body, &ev) != nil || ev.Event != EventStateChanged || ev.Seq != 1 || ev.Session != "open" {
			t.Fatalf("connection %d: event = %s", conn, body)
		}
		cls.set("claude", "agent:claude", ops.PaneStateStable)
		cancel()
		_ = hub.Close() // the link drops; the connector reconnects
	}
}

// A write accepted before the shutdown finishes before the modules that start
// earlier (the core with the store and the host) are stopped: the module's
// stop hook waits for the op server.
func TestOpServerModuleStopWaitsForWritesInFlight(t *testing.T) {
	dir := t.TempDir()
	host := newFakeHost()
	core := coreDeps{
		openStore:  func(ctx context.Context, d string) (*store.Store, error) { return store.OpenContext(ctx, d) },
		newHost:    func(string) (term.SessionHost, error) { return host, nil },
		classifier: newFakeClassifier(),
	}
	r := NewRegistry()
	_ = r.Register(ModuleCore, OrderCore, core.start)
	_ = r.Register(ModuleConnector, OrderConnector, connectorDeps{dialer: &pairDialer{hubs: make(chan *transport.Session, 1)}}.start)
	_ = r.Register(ModuleOpServer, OrderOpServer, opServerDeps{}.start)
	rt := NewRuntime(r, Options{StopTimeout: 5 * time.Second})
	err := rt.Start(context.Background(), Settings{
		WorkerCore: config.WorkerCore{StateDir: dir, TmuxSocket: filepath.Join(dir, "tmux.sock"), Name: "w"},
		Logger:     slog.New(slog.NewTextHandler(&logBuf{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, ok := Lookup[*OpServer](rt)
	if !ok {
		t.Fatal("no op server service")
	}
	if why := srv.admit(); why != nil { // an op stream in flight
		t.Fatal(why)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- rt.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("the runtime stopped while a write was in flight (%v)", err)
	case <-time.After(150 * time.Millisecond):
	}
	if host.closes != 0 {
		t.Fatal("the host was closed under a write in flight")
	}
	srv.release()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the runtime did not stop after the write finished")
	}
}

// The event publisher is a service: a later module registers for the Hub's
// frames with Require[*EventPublisher] and no edit of this module.
func TestEventPublisherIsAServiceForFrameRegistration(t *testing.T) {
	dir := t.TempDir()
	core := coreDeps{
		openStore:  func(ctx context.Context, d string) (*store.Store, error) { return store.OpenContext(ctx, d) },
		newHost:    func(string) (term.SessionHost, error) { return newFakeHost(), nil },
		classifier: newFakeClassifier(),
	}
	r := NewRegistry()
	_ = r.Register(ModuleCore, OrderCore, core.start)
	_ = r.Register(ModuleConnector, OrderConnector, connectorDeps{dialer: &pairDialer{hubs: make(chan *transport.Session, 1)}}.start)
	_ = r.Register(ModuleOpServer, OrderOpServer, opServerDeps{}.start)
	registered := false
	_ = r.Register("later", OrderOpServer+10, func(_ context.Context, env *Env) (StopFunc, error) {
		pub, err := Require[*EventPublisher](env)
		if err != nil {
			return nil, err
		}
		pub.OnFrame(proto.TypeTurnAck, func(json.RawMessage) {})
		registered = true
		return nil, nil
	})
	rt := NewRuntime(r, Options{})
	err := rt.Start(context.Background(), Settings{
		WorkerCore: config.WorkerCore{StateDir: dir, TmuxSocket: filepath.Join(dir, "tmux.sock"), Name: "w"},
		Logger:     slog.New(slog.NewTextHandler(&logBuf{}, nil)),
	})
	if err != nil || !registered {
		t.Fatalf("start = %v, registered = %v", err, registered)
	}
	_ = rt.Stop(context.Background())
}
