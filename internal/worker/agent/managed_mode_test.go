package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// The Android managed mode keeps the tmux wire kind, reports the sessions as
// hidden with the fixed Android reason, and never leaks the socket path into
// the hello.
func TestManagedAndroidSessionHostIsHiddenTmux(t *testing.T) {
	skipWindows(t)
	s := Settings{WorkerCore: config.WorkerCore{TmuxSocket: "/data/user/0/app/no_backup/run/tmux.sock",
		Managed: config.ManagedHost{Android: true, TmuxBinary: "/data/app/lib/libtmux.so"}}}
	spec, info, err := sessionHostSpec(s)
	if err != nil {
		t.Fatal(err)
	}
	if spec != s.TmuxSocket || info.Kind != transport.SessionHostTmux || !info.Hidden || info.Code != transport.HostReasonAndroidManaged {
		t.Fatalf("spec %q info %+v", spec, info)
	}
	var f transport.WorkerFacts
	info.applyTo(&f)
	if !f.SessionsHidden || f.SessionHost != "tmux" || f.SessionHostReason == "" || strings.Contains(f.SessionHostReason, "/data") {
		t.Errorf("facts = %+v", f)
	}
	if !strings.Contains(info.localReason(), "/data/user/0") {
		t.Errorf("local reason %q lacks the socket", info.localReason())
	}
}

func TestManagedUpdateBlock(t *testing.T) {
	if got := ManagedUpdateBlock(config.ManagedHost{}); got != "" {
		t.Errorf("ordinary host blocked: %q", got)
	}
	if got := ManagedUpdateBlock(config.ManagedHost{TmuxBinary: "/x/tmux"}); got != "" {
		t.Errorf("a tmux binary alone blocked the update: %q", got)
	}
	if got := ManagedUpdateBlock(config.ManagedHost{Android: true}); !strings.Contains(got, "APK") {
		t.Errorf("android block = %q", got)
	}
}

// The core module builds the tmux host through the managed factory, with the
// spec, whenever a binary or the Android mode is set; the plain factory
// otherwise.
func TestCoreUsesTheManagedHostFactory(t *testing.T) {
	skipWindows(t)
	for name, tc := range map[string]struct {
		m       config.ManagedHost
		managed bool
	}{
		"ordinary":    {config.ManagedHost{}, false},
		"binary only": {config.ManagedHost{TmuxBinary: "/opt/tmux"}, true},
		"android":     {config.ManagedHost{Android: true, TmuxBinary: "/data/app/lib/libtmux.so"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			var plain, managed int
			var got config.ManagedHost
			host := newFakeHost()
			d := coreDeps{
				openStore:   func(ctx context.Context, dir string) (*store.Store, error) { return store.OpenContext(ctx, dir) },
				newHost:     func(string) (term.SessionHost, error) { plain++; return host, nil },
				managedHost: func(_ string, m config.ManagedHost) (term.SessionHost, error) { managed++; got = m; return host, nil },
				hostSpec:    sessionHostSpec,
				classifier:  newFakeClassifier(),
			}
			r := NewRegistry()
			if err := r.Register(ModuleCore, OrderCore, d.start); err != nil {
				t.Fatal(err)
			}
			rt := NewRuntime(r, Options{})
			dir := t.TempDir()
			if err := rt.Start(context.Background(), Settings{WorkerCore: config.WorkerCore{
				StateDir: dir, TmuxSocket: filepath.Join(dir, "tmux.sock"), Name: "w", Managed: tc.m}}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = rt.Stop(context.Background()) })
			if tc.managed && (managed != 1 || plain != 0 || got != tc.m) {
				t.Errorf("managed=%d plain=%d spec=%+v", managed, plain, got)
			}
			if !tc.managed && (managed != 0 || plain != 1) {
				t.Errorf("managed=%d plain=%d", managed, plain)
			}
			info, _ := lookupService[*SessionHostInfo](rt)
			if tc.m.Android && (info == nil || !info.Hidden || info.Code != transport.HostReasonAndroidManaged) {
				t.Errorf("info = %+v", info)
			}
		})
	}
}

// The code of a managed Worker is in the private status only: not on the
// console, not in the log, not in the legacy status field.
func TestManagedPairingCodeIsPrivate(t *testing.T) {
	skipWindows(t)
	out, pd, logs, rt := startAutoPairRuntimeOpts(t, map[string]string{"MINION_SERVER": "https://hub.example"}, nil, true)
	priv, ok := lookupService[*PairingStatus](rt)
	if !ok || priv == nil {
		t.Fatal("no private pairing status")
	}
	var cur PairingPresentation
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, ok := priv.Current(); ok {
			cur = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no private code; calls=%d", pd.nCalls())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !codeRE.MatchString(cur.Code) || cur.WorkerWords == "" || cur.WorkerFingerprint == "" || cur.ExpiresAt == "" || cur.HubVerified {
		t.Fatal("private presentation is missing required fields")
	}
	if codeRE.MatchString(out.String()) || strings.Contains(out.String(), cur.Code) {
		t.Error("the code reached the console")
	}
	if strings.Contains(logs.String(), cur.Code) || codeRE.MatchString(logs.String()) {
		t.Error("the code reached the log")
	}
	// Send the same frame as the Android app on the actual owner-only socket.
	socket := filepath.Join(rt.settings.StateDir, "local.sock")
	if err := localapi.VerifySocket(socket); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("unix", socket, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteFrame(conn, proto.StreamFrameLimit,
		localapi.RequestFrame{Type: proto.TypeOpRequest, Op: OpLocalStatus}); err != nil {
		t.Fatal(err)
	}
	raw, err := proto.ReadFrame(conn, proto.StreamFrameLimit)
	if err != nil {
		t.Fatal(err)
	}
	var frame localapi.ResultFrame
	if err := json.Unmarshal(raw, &frame); err != nil || frame.Type != proto.TypeOpResult {
		t.Fatal("expected an op.result from the local socket")
	}
	var reply LiveStatusReply
	if err := json.Unmarshal(frame.Result, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.PendingPairingCode != "" || reply.Pairing == nil || reply.Pairing.Code != cur.Code ||
		!reply.PairingKnown || reply.StateDir != rt.settings.StateDir {
		t.Fatal("local reply did not isolate the pairing code in the private field")
	}

}

func TestPairingStatusExpiresAndClears(t *testing.T) {
	clk := &fakeClockAt{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s := NewPairingStatus(clk)
	if _, ok := s.Current(); ok {
		t.Fatal("empty status has a code")
	}
	s.set(PairingPresentation{Code: "MN-AAAA-BBBB", expires: clk.now.Add(time.Minute)})
	if _, ok := s.Current(); !ok {
		t.Fatal("fresh code missing")
	}
	clk.now = clk.now.Add(time.Minute)
	if _, ok := s.Current(); ok {
		t.Error("an expired code is still shown")
	}
	s.set(PairingPresentation{Code: "MN-AAAA-BBBB", expires: clk.now.Add(time.Minute)})
	s.Clear()
	if _, ok := s.Current(); ok {
		t.Error("a cleared code is still shown")
	}
	var nilStatus *PairingStatus
	nilStatus.Clear() // no panic
	if _, ok := nilStatus.Current(); ok {
		t.Error("nil status has a code")
	}
}

// fakeClockAt is a transport.Clock at a time the test sets; only Now is used.
type fakeClockAt struct {
	transport.Clock
	now time.Time
}

func (c *fakeClockAt) Now() time.Time { return c.now }

// The managed Android host is for Unix systems; Windows hosts its sessions
// with psmux or ConPTY.
func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the managed Android host is not available on Windows")
	}
}

// A create on a managed host that is down is a plain failure, not a cached
// unknown: the same request works after the supervisor restarted the server.
func TestSessionCreateOnADownManagedHostIsNotCachedUnknown(t *testing.T) {
	e := newEnv(t)
	e.host.createErr = fmt.Errorf("tmux: the managed server is not running: %w", term.ErrHostDown)
	args := ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}}
	if _, err := e.ex.Execute(context.Background(), Request{Op: ops.OpSessionCreate, Args: mustJSON(t, args)}); err == nil {
		t.Fatal("a create on a down host succeeded")
	}
	e.host.createErr = nil
	res := e.mustWrite(ops.OpSessionCreate, args)
	if res.Outcome == ops.OutcomeUnknown {
		t.Fatalf("the failure was cached as unknown: %+v", res)
	}
}

func TestManagedPairingRequiresTheCodeWithOrWithoutAnExpectedFingerprint(t *testing.T) {
	skipWindows(t)
	hub, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, pin := range []string{"", hub.Fingerprint().String()} {
		out, pd, _, _ := startAutoPairRuntimeOpts(t, map[string]string{
			"MINION_SERVER": "https://hub.example", "MINION_HUB_FINGERPRINT": pin,
		}, nil, true)
		eventually(t, func() bool { return pd.nCalls() != 0 })
		if pd.call(0).AllowAdmit {
			t.Fatal("the managed Worker accepted code-less admission")
		}
		if codeRE.MatchString(out.String()) {
			t.Fatal("the managed Worker printed a pairing code")
		}
	}
}

func TestPrivatePairingUsesTheRestoredExpiryAndIgnoresAClosedAttempt(t *testing.T) {
	clk := &fakeClockAt{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	status := NewPairingStatus(clk)
	p := &Pairer{cfg: PairerConfig{Private: status}, ttl: PairCodeTTL}
	deadline := clk.now.Add(10 * time.Second)
	life := &codeLife{
		code: identity.NewPairingCode(), restored: true, resume: &store.PairResume{ExpiresAt: deadline},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	box := &codeBox{p: p, life: life, ctx: ctx, held: true}
	life.show(clk.now, p.ttl)
	box.flush()
	got, ok := status.Current()
	if !ok || got.ExpiresAt != deadline.Format(time.RFC3339Nano) {
		t.Fatal("the displayed deadline differs from the resumed code lifetime")
	}
	status.Clear()
	if err := box.hubHello([]byte{1}, false); err == nil {
		t.Fatal("a retired attempt accepted a late Hub hello")
	}
	box.print()
	if _, ok := status.Current(); ok {
		t.Fatal("a late callback republished a retired code")
	}
	life2 := &codeLife{code: identity.NewPairingCode()}
	life2.show(clk.now, p.ttl)
	paused := &codeBox{p: p, life: life2, ctx: ctx, held: true}
	cancel()
	paused.flush()
	if _, ok := status.Current(); ok {
		t.Fatal("a paused attempt published a code")
	}
}
