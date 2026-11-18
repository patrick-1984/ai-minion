package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// countingInviteDialer counts the invitation dials.
type countingInviteDialer struct {
	mu    sync.Mutex
	inner *fakeDialer
	n     int
}

func (d *countingInviteDialer) Enroll(ctx context.Context, p InviteParams) (*InviteOutcome, error) {
	d.mu.Lock()
	d.n++
	d.mu.Unlock()
	return d.inner.Enroll(ctx, p)
}

func (d *countingInviteDialer) calls() int { d.mu.Lock(); defer d.mu.Unlock(); return d.n }

// pausedRuntime starts the core and connector modules (and the invitation
// pre-start hook) on a state directory that is paused before the start.
type pausedRuntime struct {
	rt     *Runtime
	dir    string
	pause  *PauseService
	pairs  *pairScript
	invite *countingInviteDialer
	dial   *scriptDialer
	out    *syncBuf
}

func startPausedRuntime(t *testing.T, env map[string]string, prepare func(dir string), dialer ...Dialer) *pausedRuntime {
	t.Helper()
	dir := t.TempDir()
	if _, err := SetPause(dir, true, time.Time{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if prepare != nil {
		prepare(dir)
	}
	host := newFakeHost()
	core := coreDeps{
		openStore:  func(ctx context.Context, d string) (*store.Store, error) { return store.OpenContext(ctx, d) },
		newHost:    func(string) (term.SessionHost, error) { return host, nil },
		classifier: newFakeClassifier(),
	}
	p := &pausedRuntime{dir: dir, out: &syncBuf{}, dial: &scriptDialer{},
		pairs:  &pairScript{steps: []pairStep{pairWaitStep(nil)}},
		invite: &countingInviteDialer{inner: &fakeDialer{log: &stepLog{}}}}
	hook := defaultInviteHookDeps()
	hook.dialer, hook.stdin, hook.sleep = p.invite, nil, noSleep
	r := NewRegistry()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.RegisterPreStart(HookEnrollInvite, hook.run))
	must(r.Register(ModuleCore, OrderCore, core.start))
	var connDialer Dialer = p.dial
	if len(dialer) > 0 {
		connDialer = dialer[0]
	}
	must(r.Register(ModuleConnector, OrderConnector, connectorDeps{dialer: connDialer, pairDialer: p.pairs, inviteDialer: p.invite}.start))
	res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	p.rt = NewRuntime(r, Options{})
	must(p.rt.Start(context.Background(), Settings{
		WorkerCore: config.WorkerCore{StateDir: dir, TmuxSocket: filepath.Join(dir, "tmux.sock"), Name: "w"},
		Resolver:   res, Out: p.out,
		Logger: slog.New(slog.NewTextHandler(&logBuf{}, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}))
	t.Cleanup(func() { _ = p.rt.Stop(context.Background()) })
	svc, ok := Lookup[*PauseService](p.rt)
	if !ok || !svc.Paused() {
		t.Fatal("the Worker did not start paused")
	}
	p.pause = svc
	return p
}

// Nothing is dialed before the resume: no pairing, no invitation, no
// connection, no retirement.
func (p *pausedRuntime) assertQuiet(t *testing.T) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	if n := p.pairs.nCalls(); n != 0 {
		t.Fatalf("the paused Worker paired %d times", n)
	}
	if n := p.invite.calls(); n != 0 {
		t.Fatalf("the paused Worker enrolled %d times", n)
	}
	if n := p.dial.count(); n != 0 {
		t.Fatalf("the paused Worker dialed the Hub %d times", n)
	}
}

func TestPausedBootThenResumeEnrollsInvite(t *testing.T) {
	invite, _ := newInviteKey(t)
	keyFile := filepath.Join(t.TempDir(), "invite.key")
	if err := os.WriteFile(keyFile, []byte(invite.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := startPausedRuntime(t, map[string]string{"MINION_SERVER": "https://hub.example", "MINION_INVITE_KEY_FILE": keyFile}, nil)
	if !deferredInvite(p.dir) {
		t.Fatal("the paused start did not record the deferred enrollment")
	}
	p.assertQuiet(t)
	if _, err := p.pause.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	enr, _ := Lookup[*Enrollments](p.rt)
	eventuallyTrue(t, func() bool { _, ok, _ := enr.Load(context.Background()); return ok })
	if p.invite.calls() != 1 {
		t.Fatalf("invitation dials = %d, want 1", p.invite.calls())
	}
	eventuallyTrue(t, func() bool { return !deferredInvite(p.dir) })
}

func TestPausedBootThenResumeShowsPairingCode(t *testing.T) {
	p := startPausedRuntime(t, map[string]string{"MINION_SERVER": "https://hub.example"}, nil)
	p.assertQuiet(t)
	if codeRE.MatchString(p.out.String()) {
		t.Fatal("a pairing code was shown while paused")
	}
	if _, err := p.pause.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, func() bool { return p.pairs.nCalls() > 0 && codeRE.MatchString(p.out.String()) })
}

func TestPausedBootThenResumeRetires(t *testing.T) {
	h := newRetireHub(t)
	var old RetiredIdentity
	p := startPausedRuntime(t, map[string]string{}, func(dir string) {
		rec, oldKey := retiredFor(t, h, h.url())
		h.enrol(oldKey.Public())
		if err := NewRetiredStore(dir).Add(rec); err != nil {
			t.Fatal(err)
		}
		old = rec
	}, NewHubDialer())
	time.Sleep(300 * time.Millisecond)
	if known, tomb := h.state(old.Key.Public()); !known || tomb {
		t.Fatal("the paused Worker retired the old identity")
	}
	if _, err := p.pause.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, func() bool { _, tomb := h.state(old.Key.Public()); return tomb })
}
