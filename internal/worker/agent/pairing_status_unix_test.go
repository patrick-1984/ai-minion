//go:build unix

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// pairingWorker is a real Worker (core, connector and local socket modules)
// that pairs by itself with the in-process test Hub over the production pair
// dialer.
type pairingWorker struct {
	hub  *stubPairHub
	sock *client.LocalBackend
	dir  string
	out  *syncBuf
	logs *logBuf
	rt   *Runtime
}

func startPairingWorker(t *testing.T, display string) *pairingWorker {
	t.Helper()
	return startPairingWorkerIn(t, newStubPairHub(t), t.TempDir(), display)
}

// startPairingWorkerIn starts the Worker on the state directory dir with the
// Hub h (a restart when dir was used before).
func startPairingWorkerIn(t *testing.T, h *stubPairHub, dir, display string) *pairingWorker {
	t.Helper()
	host := newFakeHost()
	core := coreDeps{
		openStore:  func(ctx context.Context, d string) (*store.Store, error) { return store.OpenContext(ctx, d) },
		newHost:    func(string) (term.SessionHost, error) { return host, nil },
		classifier: newFakeClassifier(),
	}
	sock := filepath.Join(shortRunDir(t), "minion", localapi.SocketName)
	lp := defaultLocalAPIDeps()
	lp.socketPath = func() (string, error) { return sock, nil }
	r := NewRegistry()
	for _, m := range []struct {
		name  string
		order int
		start StartFunc
	}{
		{ModuleCore, OrderCore, core.start},
		// pairDialer nil: the production dialer, against the test Hub.
		{ModuleConnector, OrderConnector, connectorDeps{dialer: &scriptDialer{}}.start},
		{ModuleLocalAPI, OrderLocalAPI, lp.start},
	} {
		if err := r.Register(m.name, m.order, m.start); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{"MINION_SERVER": h.url()}
	if display != "" {
		env["MINION_PAIRING_DISPLAY"] = display
	}
	res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	rt := NewRuntime(r, Options{})
	w := &pairingWorker{hub: h, dir: dir, out: &syncBuf{}, logs: &logBuf{}, rt: rt}
	if err := rt.Start(context.Background(), Settings{
		WorkerCore: config.WorkerCore{StateDir: dir, TmuxSocket: filepath.Join(dir, "tmux.sock"), Name: "pi"},
		Resolver:   res,
		Out:        w.out,
		Logger:     slog.New(slog.NewTextHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	w.sock = client.Local(client.WithSocketPath(sock))
	return w
}

// socketLive is what "minion worker status" asks: the live status over the
// local control socket.
type socketLive struct{ c *client.LocalBackend }

func (s socketLive) LiveStatus(ctx context.Context) (LiveStatus, error) {
	raw, err := s.c.Call(ctx, OpLocalStatus, nil)
	if err != nil {
		return LiveStatus{}, err
	}
	var reply LiveStatusReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return LiveStatus{}, err
	}
	return reply.LiveStatus()
}

// statusPairing builds the status the way "minion worker status --json" does
// and returns its pairing object.
func (w *pairingWorker) statusPairing(t *testing.T) StatusPairing {
	t.Helper()
	rep := BuildStatus(context.Background(), StatusInputs{HubURL: w.hub.url(), StateDir: w.dir, Live: socketLive{w.sock}})
	var b bytes.Buffer
	if err := rep.WriteJSON(&b); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Pairing      StatusPairing `json:"pairing"`
		Capabilities []string      `json:"capabilities"`
	}
	if err := json.Unmarshal(b.Bytes(), &out); err != nil {
		t.Fatalf("%s: %v", b.String(), err)
	}
	return out.Pairing
}

func (w *pairingWorker) pendingCode(t *testing.T, not string) StatusPairing {
	t.Helper()
	var p StatusPairing
	eventually(t, func() bool {
		p = w.statusPairing(t)
		return p.State == PairingPending && p.Code != "" && p.Code != not
	})
	return p
}

func TestRealAutoPairCodeInStatus(t *testing.T) {
	w := startPairingWorker(t, config.PairingDisplayStatus)

	first := w.pendingCode(t, "")
	if !codeRE.MatchString(first.Code) || first.Attempt == "" || first.ExpiresAt == "" {
		t.Fatalf("pending pairing = %+v", first)
	}
	if exp, err := time.Parse(time.RFC3339, first.ExpiresAt); err != nil || time.Until(exp) <= 0 || time.Until(exp) > PairCodeTTL+time.Minute {
		t.Fatalf("expires_at = %q (%v)", first.ExpiresAt, err)
	}
	eventually(t, func() bool { return w.hub.lastID() == first.Code[3:7] })

	// A wrong code typed in the Hub retires the code: the status shows the
	// new one with a new attempt.
	w.hub.claim(first.Code[:8] + otherChar(first.Code[8]) + first.Code[9:])
	second := w.pendingCode(t, first.Code)
	if second.Attempt == "" || second.Attempt == first.Attempt {
		t.Fatalf("the attempt must change with the code: %+v then %+v", first, second)
	}

	// The code read from the status pairs the Worker; the status then says none.
	eventually(t, func() bool { return w.hub.lastID() == second.Code[3:7] })
	w.hub.claim(second.Code)
	eventually(t, func() bool { return w.statusPairing(t).State == PairingNone })
	if got := w.statusPairing(t); got.Code != "" || got.Attempt != "" || got.ExpiresAt != "" {
		t.Fatalf("after pairing = %+v", got)
	}
	if w.hub.welcomed.Load() != 1 {
		t.Fatalf("welcomed = %d", w.hub.welcomed.Load())
	}
	// Neither code reached the console or the log, through rotation and
	// completion.
	for name, text := range map[string]string{"log": w.logs.String(), "stdout": w.out.String()} {
		if codeRE.MatchString(text) {
			t.Fatalf("the %s holds a pairing code:\n%s", name, text)
		}
	}
	if !strings.Contains(w.out.String(), "MINION WORKER PAIRED") {
		t.Fatalf("console:\n%s", w.out.String())
	}
}

func TestBackgroundPairingCodeNotLogged(t *testing.T) {
	w := startPairingWorker(t, config.PairingDisplayStatus)
	p := w.pendingCode(t, "")
	eventually(t, func() bool { return strings.Contains(w.out.String(), "MINION WORKER PAIRING") })
	eventually(t, func() bool { return strings.Contains(w.logs.String(), "pairing_code_shown_in=status") })
	for name, text := range map[string]string{"log": w.logs.String(), "stdout": w.out.String()} {
		if strings.Contains(text, p.Code) || codeRE.MatchString(text) {
			t.Fatalf("the %s holds the pairing code:\n%s", name, text)
		}
	}
	if !strings.Contains(w.out.String(), "shown in the tray and by minion worker") {
		t.Fatalf("the box must say where the code is:\n%s", w.out.String())
	}
}

// Without pairing_display the Worker shows the code as before (AD-20), and
// the status carries it too.
func TestConsolePairingDisplayKeepsTheCodeOnTheConsole(t *testing.T) {
	w := startPairingWorker(t, "")
	p := w.pendingCode(t, "")
	eventually(t, func() bool { return strings.Contains(w.out.String(), p.Code) })
	eventually(t, func() bool { return strings.Contains(w.logs.String(), "pairing_code="+p.Code) })
}

func TestPairingStatusSource(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	s := NewPairingStatus()
	s.now = func() time.Time { return now }
	if _, ok := s.Pending(); ok {
		t.Fatal("a new source has no code")
	}
	s.OnCode("MN-AAAA-BBBB", "a1", now.Add(time.Minute))
	if p, ok := s.Pending(); !ok || p.Code != "MN-AAAA-BBBB" || p.Attempt != "a1" {
		t.Fatalf("pending = %+v, %v", p, ok)
	}
	now = now.Add(time.Minute)
	if _, ok := s.Pending(); ok {
		t.Fatal("an expired code is not offered")
	}
	s.OnCode("MN-CCCC-DDDD", "a2", now.Add(time.Minute))
	s.OnCode("", "", time.Time{})
	if _, ok := s.Pending(); ok {
		t.Fatal("a retired code is not offered")
	}
	s.OnCode("MN-CCCC-DDDD", "a2", now.Add(time.Minute))
	s.OnDone()
	if _, ok := s.Pending(); ok {
		t.Fatal("no code after the pairing ended")
	}
}

func TestPairingStatusCapabilityIsLocal(t *testing.T) {
	found := false
	for _, f := range localFeatures {
		found = found || f == FeaturePairingStatus
	}
	if !found {
		t.Fatalf("localFeatures %v lacks %s", localFeatures, FeaturePairingStatus)
	}
}

// Gate 1: a pairing that a background join started never shows its code on the
// console or in the Worker log, also when a flag or the environment asks for
// pairing_display=console.
func TestBackgroundJoinPairingIgnoresConsoleDisplay(t *testing.T) {
	h := newStubPairHub(t)
	dir := t.TempDir()
	if err := WriteJoinPin(dir, JoinPin{Version: JoinPinVersion, Source: JoinPinSourceBackground, Server: h.url(),
		ServerCreated: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	w := startPairingWorkerIn(t, h, dir, config.PairingDisplayConsole)
	p := w.pendingCode(t, "") // the code is in the status
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(w.out.String(), p.Code) || strings.Contains(w.logs.String(), p.Code) {
		t.Fatalf("the background join's code reached the console or the log:\n%s\n%s", w.out.String(), w.logs.String())
	}
}
