package agent

import (
	"encoding/json"
	"errors"
	"github.com/patrick-1984/ai-minion/internal/identity"

	"context"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pauseFixture(t *testing.T, paused bool) (*connFixture, *PauseService) {
	t.Helper()
	f := newConnFixture(t)
	if err := WritePause(f.dir, paused); err != nil {
		t.Fatal(err)
	}
	ps := NewPauseService(f.dir, nil)
	f.conn = f.newConnector(func(c *ConnectorConfig) { c.Pause = ps.Gate() })
	ps.settled = func() bool {
		st := f.conn.Status()
		return st.State == ConnIdle && st.Reason == ReasonPaused
	}
	return f, ps
}

// A pause closes the Hub link before it is acknowledged, no dial follows
// while it lasts, and a resume connects again.
func TestPauseClosesTheLinkAndGatesReconnect(t *testing.T) {
	f, ps := pauseFixture(t, false)
	sess := newFakeSession()
	f.dial.script(f.welcomeStep(sess, okWelcome()), f.welcomeStep(newFakeSession(), okWelcome()))
	f.run()
	f.waitState(ConnConnected)

	if err := ps.Set(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	// Acknowledged: the link is closed and the connector rests as paused.
	if st := f.conn.Status(); st.State != ConnIdle || st.Reason != ReasonPaused {
		t.Fatalf("after the ack the connector is %+v", st)
	}
	select {
	case <-sess.Done():
	default:
		t.Fatal("the Hub link was not closed before the ack")
	}
	time.Sleep(100 * time.Millisecond)
	f.clk.Advance(time.Hour)
	if n := f.dial.count(); n != 1 {
		t.Fatalf("%d dials; a paused Worker must not reconnect", n)
	}
	if on, err := ReadPause(f.dir); err != nil || !on {
		t.Fatalf("the pause is not saved: %v %v", on, err)
	}

	if err := ps.Set(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	f.waitState(ConnConnected)
	if n := f.dial.count(); n != 2 {
		t.Fatalf("%d dials after the resume, want 2", n)
	}
}

// A Worker that starts with a saved pause never dials: the restart keeps it.
func TestPauseSurvivesARestart(t *testing.T) {
	f, ps := pauseFixture(t, true)
	if !ps.Paused() {
		t.Fatal("the saved pause was not applied at start")
	}
	f.dial.script(f.welcomeStep(newFakeSession(), okWelcome()))
	f.run()
	f.waitIdle(ReasonPaused)
	f.clk.Advance(time.Hour)
	time.Sleep(50 * time.Millisecond)
	if n := f.dial.count(); n != 0 {
		t.Fatalf("%d dials while paused", n)
	}
}

// Automatic pairing counts as Hub work: a pause waits for it to end, and work
// that starts after the pause is refused.
func TestPauseWaitsForHubWork(t *testing.T) {
	_, ps := pauseFixture(t, false)
	ps.settled = nil
	wctx, end, ok := ps.Gate().beginWork(context.Background())
	if !ok {
		t.Fatal("work refused while not paused")
	}
	done := make(chan error, 1)
	go func() { done <- ps.Set(context.Background(), true) }()
	select {
	case err := <-done:
		t.Fatalf("the pause was acknowledged with Hub work running: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if wctx.Err() == nil {
		t.Fatal("the pause did not end the Hub work")
	}
	if !end() {
		t.Fatal("end did not report the pause")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ps.Gate().beginWork(context.Background()); ok {
		t.Fatal("Hub work was admitted while paused")
	}
}

// A link that cannot be closed in time keeps the Worker paused but fails the
// call: a pause is never reported as done when it is not.
func TestPauseNotAckedWhenTheLinkStays(t *testing.T) {
	_, ps := pauseFixture(t, false)
	ps.settled = func() bool { return false }
	ps.settle = 80 * time.Millisecond
	if err := ps.Set(context.Background(), true); err == nil || !ps.Paused() {
		t.Fatalf("err=%v paused=%v", err, ps.Paused())
	}
}

// A pause that cannot be saved changes nothing.
func TestPauseNotSavedChangesNothing(t *testing.T) {
	_, ps := pauseFixture(t, false)
	ps.dir = filepath.Join(ps.dir, "missing", "\x00bad")
	if err := ps.Set(context.Background(), true); err == nil || ps.Paused() {
		t.Fatalf("err=%v paused=%v", err, ps.Paused())
	}
}

func TestUnreadablePauseFileStaysPaused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PauseFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !NewPauseService(dir, nil).Paused() {
		t.Fatal("a pause file that cannot be read resumed the Worker")
	}
	if _, err := ReadPause(dir); err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("err = %v", err)
	}
}

// The local op pauses the Worker; local control keeps working, an open
// session is untouched, and a request that needs the Hub is refused with the
// paused code while nothing reaches the Hub.
func TestLocalPauseOpKeepsLocalControl(t *testing.T) {
	e, l, h := fwdEnv(t, struct{}{})
	ps := NewPauseService(t.TempDir(), nil)
	l.pause = ps
	e.host.addSession("work", "bash")
	call := func(op string, args any) (json.RawMessage, error) {
		return l.Execute(context.Background(), localapiRequest(op, mustJSON(t, args)), func(json.RawMessage) {})
	}
	if _, err := call(OpLocalPause, map[string]bool{"paused": true}); err != nil || !ps.Paused() {
		t.Fatalf("pause op: err=%v paused=%v", err, ps.Paused())
	}
	// Local control still answers, and the session is still there.
	if _, err := call(ops.OpSessionSnapshot, ops.SessionSnapshotArgs{Target: ops.Target{SessionName: "work"}}); err != nil {
		t.Fatalf("local snapshot while paused: %v", err)
	}
	foreign := ops.SessionSnapshotArgs{Target: ops.Target{WorkerID: ops.NewUUIDv7(), SessionName: "x"}}
	if _, err := call(ops.OpSessionSnapshot, foreign); proto.CodeOf(err) != proto.CodePaused {
		t.Fatalf("a Hub request while paused: %v", err)
	}
	h.mu.Lock()
	n := len(h.requests)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d requests reached the Hub while paused", n)
	}
	if _, err := call(OpLocalPause, map[string]bool{"paused": false}); err != nil || ps.Paused() {
		t.Fatalf("resume op: err=%v paused=%v", err, ps.Paused())
	}
	if _, err := call(OpLocalPause, map[string]string{"x": "y"}); proto.CodeOf(err) != proto.CodeInvalidArgument {
		t.Fatalf("bad args: %v", err)
	}
}

// A pause that follows a resume must wait for the attempt that the resume
// admitted (it is loading its key), and no dial happens after it.
func TestSecondPauseWaitsForTheAdmittedAttempt(t *testing.T) {
	f := newConnFixture(t)
	if err := WritePause(f.dir, true); err != nil {
		t.Fatal(err)
	}
	ps := NewPauseService(f.dir, nil)
	inKey, release := make(chan struct{}, 1), make(chan struct{})
	f.conn = f.newConnector(func(c *ConnectorConfig) {
		c.Pause = ps.Gate()
		load := c.Key
		c.Key = func() (*identity.Key, error) {
			inKey <- struct{}{}
			<-release
			return load()
		}
	})
	ps.settled = func() bool {
		st := f.conn.Status()
		return st.State == ConnIdle && st.Reason == ReasonPaused
	}
	f.dial.script(f.welcomeStep(newFakeSession(), okWelcome()))
	f.run()
	f.waitIdle(ReasonPaused) // paused and settled, as after a first pause

	if err := ps.Set(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	<-inKey // the attempt is admitted and sits in the key load
	done := make(chan error, 1)
	go func() { done <- ps.Set(context.Background(), true) }()
	select {
	case err := <-done:
		t.Fatalf("the second pause was acknowledged with an attempt in flight: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := f.dial.count(); n != 0 {
		t.Fatalf("%d dials after the pause", n)
	}
}

// An unreadable or empty pause file keeps the Worker paused.
func TestPauseFileFailsClosed(t *testing.T) {
	for name, body := range map[string]string{"empty object": `{}`, "null": `null`, "wrong type": `{"paused":"no"}`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, PauseFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if on, err := ReadPause(dir); !on || err == nil {
			t.Errorf("%s: paused=%v err=%v", name, on, err)
		}
		if !NewPauseService(dir, nil).Paused() {
			t.Errorf("%s: the Worker started resumed", name)
		}
	}
	// A pause file that cannot be read (here: a directory).
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, PauseFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if on, err := ReadPause(dir); !on || err == nil {
		t.Errorf("unreadable: paused=%v err=%v", on, err)
	}
	if on, err := ReadPause(t.TempDir()); on || err != nil {
		t.Errorf("missing file: paused=%v err=%v", on, err)
	}
}

// An invitation enrollment reads the pause right before each dial: after a
// pause nothing reaches the Hub, and the failure is final (no retry).
func TestInviteDialIsGuardedByThePause(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	inner := inviteDialerFunc(func(context.Context, InviteParams) (*InviteOutcome, error) {
		calls++
		return nil, errors.New("reached the Hub")
	})
	g := pauseGuardDialer{inner: inner, stateDir: dir}
	if _, err := g.Enroll(context.Background(), InviteParams{}); err == nil || calls != 1 {
		t.Fatalf("unpaused: err=%v calls=%d", err, calls)
	}
	if err := WritePause(dir, true); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Enroll(context.Background(), InviteParams{}); !errors.Is(err, errEnrollPaused) || calls != 1 {
		t.Fatalf("paused: err=%v calls=%d", err, calls)
	}
	if !permanentInviteError(errEnrollPaused) {
		t.Fatal("a paused refusal would be retried")
	}
}

type inviteDialerFunc func(context.Context, InviteParams) (*InviteOutcome, error)

func (f inviteDialerFunc) Enroll(ctx context.Context, p InviteParams) (*InviteOutcome, error) {
	return f(ctx, p)
}

// A Worker that is not enrolled has nothing to pause: the op refuses (setup
// or an enrollment may still be running), and an enrolled one pauses.
func TestPauseRefusedBeforeEnrollment(t *testing.T) {
	f := newConnFixture(t) // enrolled
	ps := NewPauseService(t.TempDir(), nil)
	l := localExecutor{ex: newEnv(t).ex, log: discardLogger(), pause: ps, enrollments: f.enr}
	call := func() error {
		_, err := l.Execute(context.Background(), localapiRequest(OpLocalPause, mustJSON(t, map[string]bool{"paused": true})), func(json.RawMessage) {})
		return err
	}
	if err := call(); err != nil || !ps.Paused() {
		t.Fatalf("enrolled: err=%v paused=%v", err, ps.Paused())
	}
	ps2 := NewPauseService(t.TempDir(), nil)
	e2 := newEnv(t)
	l = localExecutor{ex: e2.ex, log: discardLogger(), pause: ps2, enrollments: NewEnrollments(e2.st, t.TempDir())}
	err := call()
	if proto.CodeOf(err) != proto.CodeConflict || ps2.Paused() {
		t.Fatalf("not enrolled: err=%v paused=%v", err, ps2.Paused())
	}
}
