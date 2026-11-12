//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// startStatusWorker runs the core, connector and local socket modules, the
// way the Worker orders them, and returns a client on the socket.
func startStatusWorker(t *testing.T, hubURL string, hub *identity.Key) (*client.LocalBackend, *connRun) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	enrolledState(t, hub, false)(dir, st)
	_ = st.Close()

	host := newFakeHost()
	core := coreDeps{
		openStore:  func(ctx context.Context, d string) (*store.Store, error) { return store.OpenContext(ctx, d) },
		newHost:    func(string) (term.SessionHost, error) { return host, nil },
		classifier: newFakeClassifier(),
	}
	sock := filepath.Join(shortRunDir(t), "minion", localapi.SocketName)
	lp := defaultLocalAPIDeps()
	lp.socketPath = func() (string, error) { return sock, nil }
	dialer := &scriptDialer{}
	r := NewRegistry()
	for _, m := range []struct {
		name  string
		order int
		start StartFunc
	}{
		{ModuleCore, OrderCore, core.start},
		{ModuleConnector, OrderConnector, connectorDeps{dialer: dialer}.start},
		{ModuleLocalAPI, OrderLocalAPI, lp.start},
	} {
		if err := r.Register(m.name, m.order, m.start); err != nil {
			t.Fatal(err)
		}
	}
	res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) {
		if k == "MINION_SERVER" {
			return hubURL, hubURL != ""
		}
		return "", false
	}}
	rt := NewRuntime(r, Options{})
	err = rt.Start(context.Background(), Settings{
		WorkerCore: config.WorkerCore{StateDir: dir, TmuxSocket: filepath.Join(dir, "tmux.sock"), Name: "w"},
		Resolver:   res,
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	return client.Local(client.WithSocketPath(sock)), &connRun{rt: rt, host: host, dir: dir, dialer: dialer}
}

func statusReply(t *testing.T, c *client.LocalBackend) LiveStatusReply {
	t.Helper()
	raw, err := c.Call(context.Background(), OpLocalStatus, nil)
	if err != nil {
		t.Fatal(err)
	}
	var reply LiveStatusReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return reply
}

// The local socket answers with the connector state of the running Worker:
// connected, then retrying with the last error.
func TestLocalSocketServesTheLiveConnectorStatus(t *testing.T) {
	hub, _ := identity.GenerateKey()
	c, run := startStatusWorker(t, "https://hub.example:8443", hub)
	var mu sync.Mutex
	sess := newFakeSession()
	run.dialer.script(func(p ConnectParams) (*Attempt, error) {
		mu.Lock()
		defer mu.Unlock()
		return &Attempt{
			Pin: p.Pin, OpPub: p.Pin.Pub, TranscriptHash: randomHash(), Version: proto.Current,
			Reply:   Reply{Type: proto.TypeWelcome, Welcome: okWelcome()},
			Session: sess,
		}, nil
	})
	conn, _ := Lookup[*Connector](run.rt)
	eventually(t, func() bool { return conn.Status().State == ConnConnected })

	got := statusReply(t, c)
	if got.State != string(ConnConnected) || got.ConnectedSince == "" || got.StateDir != run.dir {
		t.Fatalf("connected reply = %+v", got)
	}
	if got.PairingKnown || got.PendingPairingCode != "" {
		t.Fatalf("the Worker has no pairing source, the reply must not claim one: %+v", got)
	}
	live, err := got.LiveStatus()
	if err != nil || live.Connector.State != ConnConnected || live.Connector.ConnectedSince.IsZero() {
		t.Fatalf("LiveStatus() = %+v, %v", live, err)
	}

	run.dialer.script(func(ConnectParams) (*Attempt, error) { return nil, errors.New("dial hub: connection refused") })
	sess.drop(errors.New("connection reset"))
	eventually(t, func() bool { return conn.Status().LastError != "" })
	got = statusReply(t, c)
	if got.State == string(ConnConnected) || got.LastError == "" || got.LastErrorAt == "" {
		t.Fatalf("retrying reply = %+v", got)
	}
}

// A Worker without a connector still answers: the connector is idle.
func TestLocalSocketStatusWithoutAConnectorIsIdle(t *testing.T) {
	e := newEnv(t)
	c, _, _ := startLocalWorker(t, e)
	got := statusReply(t, c)
	if got.PairingKnown {
		t.Fatalf("reply = %+v", got)
	}
	if live, err := got.LiveStatus(); err != nil || live.Connector.State != "" {
		t.Fatalf("LiveStatus() = %+v, %v", live, err)
	}
}

// OpLocalStatus is local only: the Executor, which the Hub reaches, does not
// know it, and the Hub never hears of it.
func TestLocalStatusOpIsNotAnExecutorOp(t *testing.T) {
	e := newEnv(t)
	for _, name := range e.ex.Ops() {
		if name == OpLocalStatus {
			t.Fatalf("the executor announces %s to the Hub", name)
		}
	}
	_, err := e.ex.Execute(context.Background(), Request{Op: OpLocalStatus})
	if ae, ok := AsAPIError(err); !ok || ae.Code != proto.CodeUnsupportedOp {
		t.Fatalf("Execute(%s) = %v", OpLocalStatus, err)
	}
}

func TestLiveStatusReplyRejectsAnInvalidTime(t *testing.T) {
	if _, err := (LiveStatusReply{ConnectedSince: "yesterday"}).LiveStatus(); err == nil {
		t.Fatal("an invalid time was accepted")
	}
}

func TestLiveStatusSourceReportsWhetherItKnowsThePairingCode(t *testing.T) {
	none, _ := LiveStatusSource{}.LiveStatus(context.Background())
	if none.PairingKnown {
		t.Fatal("a source without a pairing function claims to know")
	}
	idle, _ := LiveStatusSource{Pairing: func() (string, bool) { return "", false }}.LiveStatus(context.Background())
	if !idle.PairingKnown || idle.PendingPairingCode != "" {
		t.Fatalf("known and none = %+v", idle)
	}
	pend, _ := LiveStatusSource{Pairing: func() (string, bool) { return "MN-AAAA-BBBB", true }}.LiveStatus(context.Background())
	if !pend.PairingKnown || pend.PendingPairingCode != "MN-AAAA-BBBB" {
		t.Fatalf("pending = %+v", pend)
	}
}
