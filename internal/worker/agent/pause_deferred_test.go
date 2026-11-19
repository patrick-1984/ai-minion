package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	hook  func(context.Context, InviteParams) (*InviteOutcome, error)
}

func (d *countingInviteDialer) Enroll(ctx context.Context, p InviteParams) (*InviteOutcome, error) {
	d.mu.Lock()
	d.n++
	hook := d.hook
	d.mu.Unlock()
	if hook != nil {
		return hook(ctx, p)
	}
	return d.inner.Enroll(ctx, p)
}

func (d *countingInviteDialer) setHook(h func(context.Context, InviteParams) (*InviteOutcome, error)) {
	d.mu.Lock()
	d.hook = h
	d.mu.Unlock()
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
	return startPausedRuntimeOpts(t, env, nil, nil, prepare, dialer...)
}

// startPausedRuntimeStdin starts a paused Worker whose --invite-key is - and
// whose stdin is stdin.
func startPausedRuntimeStdin(t *testing.T, stdin io.Reader) *pausedRuntime {
	t.Helper()
	return startPausedRuntimeOpts(t, map[string]string{"MINION_SERVER": "https://hub.example"},
		config.MapSource{config.KeyInviteKey: "-"}, stdin, nil)
}

func startPausedRuntimeOpts(t *testing.T, env map[string]string, flags config.FlagSource, stdin io.Reader, prepare func(dir string), dialer ...Dialer) *pausedRuntime {
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
	must(r.Register(ModuleConnector, OrderConnector, connectorDeps{dialer: connDialer, pairDialer: p.pairs, inviteDialer: p.invite, inviteStdin: stdin}.start))
	res := &config.Resolver{Role: config.ScopeWorker, Flags: flags, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
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

// Gate 1: a second pause during the deferred enrollment's handshake ends the
// attempt before the pause is acknowledged; the enrollment runs again after
// the next resume.
func TestSecondPauseEndsDeferredEnrollment(t *testing.T) {
	invite, _ := newInviteKey(t)
	keyFile := filepath.Join(t.TempDir(), "invite.key")
	if err := os.WriteFile(keyFile, []byte(invite.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := startPausedRuntime(t, map[string]string{"MINION_SERVER": "https://hub.example", "MINION_INVITE_KEY_FILE": keyFile}, nil)
	var ended atomic.Int32
	started := make(chan struct{}, 4)
	p.invite.setHook(func(ctx context.Context, _ InviteParams) (*InviteOutcome, error) {
		started <- struct{}{}
		<-ctx.Done()
		ended.Add(1)
		return nil, ctx.Err()
	})
	if _, err := p.pause.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the enrollment did not start after the resume")
	}
	if _, err := p.pause.Pause(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if ended.Load() != 1 {
		t.Fatal("the pause was acknowledged while the enrollment handshake still ran")
	}
	calls := p.invite.calls()
	time.Sleep(300 * time.Millisecond)
	if p.invite.calls() != calls {
		t.Fatal("the paused Worker dialed the Hub again")
	}
	if !deferredInvite(p.dir) {
		t.Fatal("the pending enrollment was dropped by the pause")
	}
	p.invite.setHook(nil)
	if _, err := p.pause.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	enr, _ := Lookup[*Enrollments](p.rt)
	eventuallyTrue(t, func() bool { _, ok, _ := enr.Load(context.Background()); return ok })
}

// Gate 1: --invite-key - read on a paused start's resume: the key comes from
// the Worker's stdin, once, and survives a pause-aware retry.
func TestPausedBootThenResumeEnrollsInviteFromStdin(t *testing.T) {
	invite, _ := newInviteKey(t)
	p := startPausedRuntimeStdin(t, strings.NewReader(invite.String()+"\n"))
	if _, err := p.pause.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	enr, _ := Lookup[*Enrollments](p.rt)
	eventuallyTrue(t, func() bool { _, ok, _ := enr.Load(context.Background()); return ok })
}

func TestCachedKeyReaderServesTheSameLineAgain(t *testing.T) {
	c := newCachedKeyReader(strings.NewReader("key-line\nrest\n"))
	for range 2 {
		b, err := io.ReadAll(c.reader())
		if err != nil || string(b) != "key-line\n" {
			t.Fatalf("read = %q, %v", b, err)
		}
	}
}
