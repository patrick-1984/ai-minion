package agent

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// syncBuf is a goroutine-safe console.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startAutoPairRuntime starts the core and connector modules with a scripted
// pairing dialer and returns the console and the dialer.
func startAutoPairRuntime(t *testing.T, env map[string]string, prepare func(string, *store.Store)) (*syncBuf, *pairScript) {
	t.Helper()
	out, pd, _, _ := startAutoPairRuntimeOpts(t, env, prepare, false)
	return out, pd
}

// startAutoPairRuntimeOpts is startAutoPairRuntime with the managed Android
// mode switchable; it also returns the log and the runtime.
func startAutoPairRuntimeOpts(t *testing.T, env map[string]string, prepare func(string, *store.Store), managed bool) (*syncBuf, *pairScript, *logBuf, *Runtime) {
	t.Helper()
	dir := t.TempDir()
	if prepare != nil {
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		prepare(dir, st)
		_ = st.Close()
	}
	host := newFakeHost()
	core := coreDeps{
		openStore:   func(ctx context.Context, d string) (*store.Store, error) { return store.OpenContext(ctx, d) },
		newHost:     func(string) (term.SessionHost, error) { return host, nil },
		managedHost: func(string, config.ManagedHost) (term.SessionHost, error) { return host, nil },
		classifier:  newFakeClassifier(),
	}
	pd := &pairScript{steps: []pairStep{pairWaitStep(nil)}}
	r := NewRegistry()
	if err := r.Register(ModuleCore, OrderCore, core.start); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(ModuleConnector, OrderConnector, connectorDeps{dialer: &scriptDialer{}, pairDialer: pd}.start); err != nil {
		t.Fatal(err)
	}
	if managed {
		deps := defaultLocalAPIDeps()
		deps.socketPath = func() (string, error) { return filepath.Join(dir, "local.sock"), nil }
		if err := r.Register(ModuleLocalAPI, OrderLocalAPI, deps.start); err != nil {
			t.Fatal(err)
		}
	}
	out := &syncBuf{}
	logs := &logBuf{}
	res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	rt := NewRuntime(r, Options{})
	wc := config.WorkerCore{StateDir: dir, TmuxSocket: filepath.Join(dir, "tmux.sock"), Name: "w"}
	if managed {
		wc.Managed = config.ManagedHost{Android: true, TmuxBinary: "/data/app/lib/libtmux.so"}
	}
	if err := rt.Start(context.Background(), Settings{
		WorkerCore: wc,
		Resolver:   res,
		Out:        out,
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	return out, pd, logs, rt
}

func TestUnpairedWorkerWithAHubShowsAPairingCodeByItself(t *testing.T) {
	out, pd := startAutoPairRuntime(t, map[string]string{"MINION_SERVER": "https://hub.example"}, nil)
	deadline := time.Now().Add(5 * time.Second)
	for pd.nCalls() == 0 || !codeRE.MatchString(out.String()) {
		if time.Now().After(deadline) {
			t.Fatalf("no pairing code shown; calls=%d console=%q", pd.nCalls(), out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := pd.call(0).HubURL; got != "https://hub.example" {
		t.Fatalf("paired with %q", got)
	}
}

func TestNoAutoPairingWithoutHubWithInviteKeyOrWhenEnrolled(t *testing.T) {
	hub, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		env     map[string]string
		prepare func(*testing.T) func(string, *store.Store)
	}{
		"no hub":     {env: map[string]string{}},
		"invite key": {env: map[string]string{"MINION_SERVER": "https://hub.example", "MINION_INVITE_KEY_FILE": "/nonexistent/invite.key"}},
		"enrolled": {env: map[string]string{"MINION_SERVER": "https://hub.example"},
			prepare: func(t *testing.T) func(string, *store.Store) { return enrolledState(t, hub, false) }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var prep func(string, *store.Store)
			if tc.prepare != nil {
				prep = tc.prepare(t)
			}
			out, pd := startAutoPairRuntime(t, tc.env, prep)
			time.Sleep(300 * time.Millisecond)
			if pd.nCalls() != 0 || codeRE.MatchString(out.String()) {
				t.Fatalf("paired anyway: calls=%d console=%q", pd.nCalls(), out.String())
			}
		})
	}
}
