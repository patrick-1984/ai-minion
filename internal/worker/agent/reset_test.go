package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// ---- live revoke -----------------------------------------------------------------

// liveHub is the Hub side of a real yamux session pair over an in-memory
// connection: the connector gets the Worker half through the script dialer.
type liveHub struct {
	hubSess    *transport.Session
	workerSess *transport.Session
	hash       []byte
}

func newLiveHub(t *testing.T) *liveHub {
	t.Helper()
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
	return &liveHub{hubSess: hubSess, workerSess: workerSess, hash: randomHash()}
}

// connect scripts one welcomed connection on this session pair, with the
// ServeRevokes handler registered.
func (l *liveHub) connect(f *connFixture) {
	f.dial.script(func(p ConnectParams) (*Attempt, error) {
		return &Attempt{
			Pin: p.Pin, OpPub: p.Pin.Pub, TranscriptHash: l.hash, Version: proto.Current,
			Reply: Reply{Type: proto.TypeWelcome, Welcome: okWelcome()}, Session: l.workerSess,
		}, nil
	})
	f.conn.OnLink("revoke", f.conn.ServeRevokes)
}

// sendRevoke opens an op stream, writes the first frame and returns the
// Worker's answer frame (nil when the stream ended without one).
func (l *liveHub) sendFrame(t *testing.T, frame any) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := l.hubSess.OpenOp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Reset()
	_ = st.SetDeadline(time.Now().Add(5 * time.Second))
	if err := st.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	body, err := st.ReadFrame()
	if err != nil {
		return nil
	}
	return body
}

func revokeFor(t *testing.T, signer *identity.Key, transcript []byte) map[string]any {
	t.Helper()
	sig, err := signer.Sign(identity.PurposeRevoke, transcript)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"type": proto.TypeRevoke, "sig": identity.B64(sig)}
}

func TestLiveRevokeWipesPinAndEnrollmentAndKeepsEverythingElse(t *testing.T) {
	f := newConnFixture(t)
	if err := f.st.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"work"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.NextSessionNumber(); err != nil {
		t.Fatal(err)
	}
	live := newLiveHub(t)
	live.connect(f)
	f.run()
	f.waitState(ConnConnected)

	body := live.sendFrame(t, revokeFor(t, f.hub, live.hash))
	var ack map[string]string
	if err := json.Unmarshal(body, &ack); err != nil || ack["type"] != proto.TypeOpResult || ack["op"] != "revoke" {
		t.Fatalf("answer = %s (%v), want the revoke ack", body, err)
	}
	// The ack precedes the wipe (the wipe ends the connection), so wait for
	// the connector to idle before looking at the state.
	f.waitIdle("not enrolled")
	if _, ok, err := f.enr.Load(context.Background()); err != nil || ok {
		t.Fatalf("the enrollment must be wiped: ok %v, err %v", ok, err)
	}
	if _, err := f.enr.LoadKey(); err != nil {
		t.Fatalf("a removal by the Hub keeps the key: %v", err)
	}
	if sel, err := f.st.LoadShare(); err != nil || sel.Mode != store.ShareSelect {
		t.Fatalf("share selection = %+v, %v", sel, err)
	}
	if n, err := f.st.NextSessionNumber(); err != nil || n != 2 {
		t.Fatalf("session counter = %d, %v", n, err)
	}
	// The connector ends the connection and idles, unpaired, without dialing again.
	f.waitIdle("not enrolled")
	if !strings.Contains(f.logs.String(), "removed by the hub") {
		t.Errorf("logs = %s", f.logs.String())
	}
	n := f.dial.count()
	time.Sleep(50 * time.Millisecond)
	if f.dial.count() != n {
		t.Error("a removed Worker kept dialing")
	}
}

func TestLiveRevokeThatTheHubKeyDidNotSignForThisConnectionIsIgnored(t *testing.T) {
	impostor, _ := identity.GenerateKey()
	otherTranscript := randomHash()
	cases := map[string]func(l *liveHub, f *connFixture) any{
		"another key":        func(l *liveHub, f *connFixture) any { return revokeFor(t, impostor, l.hash) },
		"another transcript": func(l *liveHub, f *connFixture) any { return revokeFor(t, f.hub, otherTranscript) },
		"wrong purpose": func(l *liveHub, f *connFixture) any {
			sig, _ := f.hub.Sign(identity.PurposeHandshakeHub, l.hash)
			return map[string]any{"type": proto.TypeRevoke, "sig": identity.B64(sig)}
		},
		"no signature": func(l *liveHub, f *connFixture) any { return map[string]any{"type": proto.TypeRevoke} },
		"bad base64":   func(l *liveHub, f *connFixture) any { return map[string]any{"type": proto.TypeRevoke, "sig": "***"} },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			f := newConnFixture(t)
			live := newLiveHub(t)
			live.connect(f)
			f.run()
			f.waitState(ConnConnected)

			body := live.sendFrame(t, mk(live, f))
			var e proto.ErrorFrame
			if err := json.Unmarshal(body, &e); err != nil || e.Type != proto.TypeError || e.Code != proto.CodeForbidden {
				t.Fatalf("answer = %s, want a forbidden error", body)
			}
			if _, ok, _ := f.enr.Load(context.Background()); !ok {
				t.Fatal("an invalid revoke deleted the enrollment")
			}
			if f.conn.Status().State != ConnConnected {
				t.Errorf("state = %s: an invalid revoke must not disturb the connection", f.conn.Status().State)
			}
		})
	}
}

func TestOtherOpStreamsAreRefusedByTheRevokeOnlyHandler(t *testing.T) {
	f := newConnFixture(t)
	live := newLiveHub(t)
	live.connect(f)
	f.run()
	f.waitState(ConnConnected)
	body := live.sendFrame(t, map[string]any{"type": proto.TypeOpRequest, "op": "sessions_list"})
	var e proto.ErrorFrame
	if err := json.Unmarshal(body, &e); err != nil || e.Code != proto.CodeUnsupportedOp {
		t.Fatalf("answer = %s", body)
	}
	if _, ok, _ := f.enr.Load(context.Background()); !ok {
		t.Fatal("an op request deleted the enrollment")
	}
}

func TestHandleRevokeOfAnEnrollmentThatWasReplacedMeanwhileDeletesNothing(t *testing.T) {
	f := newConnFixture(t)
	live := newLiveHub(t)
	live.connect(f)
	var link *Link
	got := make(chan struct{})
	var once sync.Once // the re-enrollment below makes the connector reconnect
	f.conn.OnLink("capture", func(l *Link) { once.Do(func() { link = l; close(got) }) })
	f.run()
	f.waitState(ConnConnected)
	<-got
	// A re-enrollment after this connection started moves the generation.
	if err := f.enr.Commit(context.Background(), Enrollment{Pin: identity.Pin{Pub: f.hub.Public()}, WorkerID: "new"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(revokeFor(t, f.hub, live.hash))
	if err := f.conn.HandleRevoke(link, raw, nil); err == nil {
		t.Error("a revoke for a replaced enrollment was accepted")
	}
	// A reconnect may rewrite the identity fields from its welcome, so only
	// the presence of the replacement enrollment is asserted.
	if e, ok, _ := f.enr.Load(context.Background()); !ok {
		t.Fatalf("the replacement enrollment was deleted: %+v", e)
	}
}

func TestIsRevokeFrame(t *testing.T) {
	if !IsRevokeFrame(json.RawMessage(`{"type":"revoke","sig":"x"}`)) {
		t.Error("revoke not recognised")
	}
	for _, s := range []string{`{"type":"op.request"}`, `{}`, `[]`, `nope`} {
		if IsRevokeFrame(json.RawMessage(s)) {
			t.Errorf("%s recognised as revoke", s)
		}
	}
}

// ---- reset -----------------------------------------------------------------------

type resetRig struct {
	t   *testing.T
	dir string
	st  *store.Store
	enr *Enrollments
}

func newResetRig(t *testing.T) *resetRig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &resetRig{t: t, dir: dir, st: st, enr: NewEnrollments(st, dir)}
}

// pair makes the Worker look configured: key, pin, enrollment, share
// selection, config file, session counter, result and audit rows.
func (r *resetRig) pair() *identity.Key {
	r.t.Helper()
	ctx := context.Background()
	key, err := r.enr.LoadOrCreateKey()
	if err != nil {
		r.t.Fatal(err)
	}
	hub, _ := identity.GenerateKey()
	if err := r.enr.Commit(ctx, Enrollment{Pin: identity.Pin{Pub: hub.Public()}, WorkerID: "w1", Name: "pi", Owner: "alice", Kind: "private"}); err != nil {
		r.t.Fatal(err)
	}
	if err := r.st.SaveShareContext(ctx, store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"work"}}); err != nil {
		r.t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := r.st.NextSessionNumberContext(ctx); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := r.st.PutResult(ctx, store.ResultRecord{RequestID: "req-1", Op: "send_text", Result: []byte(`{}`)}); err != nil {
		r.t.Fatal(err)
	}
	if err := r.st.AppendAudit(ctx, store.AuditRecord{Op: "send_text", Target: "work", Outcome: "delivered", RequestID: "req-1"}); err != nil {
		r.t.Fatal(err)
	}
	cfg, err := config.LoadFile(r.configPath())
	if err != nil {
		r.t.Fatal(err)
	}
	for k, v := range map[string]string{
		config.KeyServer: "https://hub.example", config.KeyHubCAFile: "/etc/hub-ca.pem", config.KeyHubInsecure: "false",
		config.KeyPreviewOnly: "true", config.KeyShareExisting: "all",
		config.KeyName: "pi", config.KeyTmuxSocket: "/run/tmux.sock",
	} {
		cfg.Set(k, v)
	}
	if err := cfg.Save(); err != nil {
		r.t.Fatal(err)
	}
	return key
}

func (r *resetRig) configPath() string { return filepath.Join(r.dir, "config.json") }

func (r *resetRig) reset() (ResetReport, error) {
	return ResetWorker(context.Background(), ResetOptions{StateDir: r.dir, Store: r.st})
}

func TestResetReturnsToTheUnconfiguredStateAndKeepsSessionsAndCounter(t *testing.T) {
	r := newResetRig(t)
	oldKey := r.pair()
	ctx := context.Background()

	rep, err := r.reset()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.WasEnrolled || !rep.ShareCleared || !rep.KeyDeleted || rep.Nothing() {
		t.Errorf("report = %+v", rep)
	}
	want := []string{config.KeyHubCAFile, config.KeyHubInsecure, config.KeyPreviewOnly, config.KeyServer, config.KeyShareExisting}
	if strings.Join(rep.ConfigKeys, ",") != strings.Join(want, ",") {
		t.Errorf("config keys removed = %v, want %v", rep.ConfigKeys, want)
	}

	// Key pair, pin, enrollment, share selection and local policy are gone.
	if _, err := r.enr.LoadKey(); err != ErrNoWorkerKey {
		t.Errorf("the key pair survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, WorkerKeyFile)); !os.IsNotExist(err) {
		t.Errorf("worker.key still exists: %v", err)
	}
	if _, ok, err := r.enr.Load(ctx); err != nil || ok {
		t.Errorf("the enrollment survived: ok %v err %v", ok, err)
	}
	if sel, err := r.st.LoadShareContext(ctx); err != nil || sel.Configured || len(sel.Sessions) != 0 {
		t.Errorf("share selection = %+v, %v", sel, err)
	}
	cfg, err := config.LoadFile(r.configPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range ResetConfigKeys {
		if _, ok, _ := cfg.Lookup(k); ok {
			t.Errorf("config key %q survived", k)
		}
	}
	for k, v := range map[string]string{config.KeyName: "pi", config.KeyTmuxSocket: "/run/tmux.sock"} {
		if got, ok, _ := cfg.Lookup(k); !ok || got != v {
			t.Errorf("machine setting %q = %q, %v; it must stay", k, got, ok)
		}
	}
	// What belongs to the tmux sessions stays: the counter continues (a name is
	// never reused) and the cache and the local audit are untouched.
	if n, err := r.st.NextSessionNumberContext(ctx); err != nil || n != 6 {
		t.Errorf("session counter continues at %d, %v; want 6", n, err)
	}
	if _, ok, err := r.st.GetResult(ctx, "req-1"); err != nil || !ok {
		t.Errorf("result cache: %v %v", ok, err)
	}

	// Re-enrolling starts from scratch with a new key.
	newKey, err := r.enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(newKey.Public(), oldKey.Public()) {
		t.Error("the re-enrollment reused the deleted key")
	}
}

func TestResetIsIdempotentAndNeverCreatesAConfigFile(t *testing.T) {
	r := newResetRig(t)
	rep, err := r.reset()
	if err != nil || !rep.Nothing() {
		t.Fatalf("reset of an unconfigured Worker: %+v, %v", rep, err)
	}
	if _, err := os.Stat(r.configPath()); !os.IsNotExist(err) {
		t.Errorf("reset created a config file: %v", err)
	}
	r.pair()
	if _, err := r.reset(); err != nil {
		t.Fatal(err)
	}
	if rep, err := r.reset(); err != nil || !rep.Nothing() {
		t.Fatalf("second reset = %+v, %v", rep, err)
	}
}

func TestResetStopsBeforeDeletingAnythingWhenTheConfigFileIsUnreadable(t *testing.T) {
	r := newResetRig(t)
	r.pair()
	if err := os.WriteFile(r.configPath(), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reset(); err == nil {
		t.Fatal("reset went on with an unreadable config file")
	}
	if _, ok, _ := r.enr.Load(context.Background()); !ok {
		t.Error("the enrollment was deleted although the reset failed up front")
	}
	if _, err := r.enr.LoadKey(); err != nil {
		t.Errorf("the key was deleted: %v", err)
	}
}

func TestResetNeedsItsInputs(t *testing.T) {
	if _, err := ResetWorker(context.Background(), ResetOptions{Store: &store.Store{}}); err == nil {
		t.Error("no state directory")
	}
	if _, err := ResetWorker(context.Background(), ResetOptions{StateDir: t.TempDir()}); err == nil {
		t.Error("no store")
	}
}

func TestResetOfARunningConnectorMakesItIdleUnpaired(t *testing.T) {
	f := newConnFixture(t)
	live := newLiveHub(t)
	live.connect(f)
	f.run()
	f.waitState(ConnConnected)

	_, err := ResetWorker(context.Background(), ResetOptions{StateDir: f.dir, Store: f.st, Enrollments: f.enr})
	if err != nil {
		t.Fatal(err)
	}
	f.waitIdle("not enrolled")
	if _, err := f.enr.LoadKey(); err != ErrNoWorkerKey {
		t.Errorf("key after reset: %v", err)
	}
	// Enrolling again with a new key connects with that key.
	newKey, err := f.enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	live2 := newLiveHub(t)
	f.dial.script(func(p ConnectParams) (*Attempt, error) {
		return &Attempt{Pin: p.Pin, OpPub: p.Pin.Pub, TranscriptHash: live2.hash, Version: proto.Current,
			Reply: Reply{Type: proto.TypeWelcome, Welcome: okWelcome()}, Session: live2.workerSess}, nil
	})
	if err := f.enr.Commit(context.Background(), Enrollment{Pin: identity.Pin{Pub: f.hub.Public()}}); err != nil {
		t.Fatal(err)
	}
	f.waitState(ConnConnected)
	last := f.dial.call(f.dial.count() - 1)
	if !bytes.Equal(last.Key.Public(), newKey.Public()) {
		t.Error("the re-enrollment did not connect with the new key")
	}
}

func TestResetThenEnrollAgainUsesANewKeyAndANewPin(t *testing.T) {
	r := newResetRig(t)
	oldKey := r.pair()
	ctx := context.Background()
	if _, err := r.reset(); err != nil {
		t.Fatal(err)
	}
	// The next "up" runs the wizard, which creates a key and commits a new
	// enrollment (Story 4.5): the same calls here.
	newKey, err := r.enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if string(newKey.Public()) == string(oldKey.Public()) {
		t.Fatal("the Worker kept its old key pair across a reset")
	}
	hub2, _ := identity.GenerateKey()
	if err := r.enr.Commit(ctx, Enrollment{Pin: identity.Pin{Pub: hub2.Public()}, WorkerID: "w2", Name: "pi", Owner: "alice", Kind: "private"}); err != nil {
		t.Fatal(err)
	}
	e, ok, err := r.enr.Load(ctx)
	if err != nil || !ok || e.WorkerID != "w2" || string(e.Pin.Pub) != string(hub2.Public()) {
		t.Fatalf("enrollment after re-enrolling = %+v ok=%v err=%v", e, ok, err)
	}
	if n, err := r.st.NextSessionNumberContext(ctx); err != nil || n != 6 {
		t.Fatalf("the session counter must continue across a reset: %d, %v", n, err)
	}
}
