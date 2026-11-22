//go:build !windows

package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The authenticated welcome of a reassigned Worker removes the previous
// owner's document at once, before the usage registration (A72) decides
// whether the link is served; a welcome of the same binding keeps it.
func TestReadbackWelcomeOfAnotherOwnerClearsBeforeConsent(t *testing.T) {
	e := newRBEnv(t)
	alice, cancel := e.link("w1", "alice", 1)
	defer cancel()
	e.rcv.LinkAdmitted(alice)
	if _, err := e.rcv.Apply(alice, e.frame(alice, 1, docA)); err != nil {
		t.Fatal(err)
	}
	e.rcv.WelcomeStored("w1", "alice", e.hubKey, false)
	if !bytes.Equal(e.onDisk(), []byte(docA)) {
		t.Fatal("a welcome of the same binding dropped the document")
	}
	e.rcv.WelcomeStored("w1", "bob", e.hubKey, false) // the registration of Bob's link is still pending
	if e.onDisk() != nil || e.pub.Usage() != nil {
		t.Fatal("Alice's document stayed readable after a welcome for Bob")
	}
	// Another Worker id, another Hub key and a pending approval clear too.
	for name, args := range map[string]func(){
		"worker":  func() { e.rcv.WelcomeStored("w2", "alice", e.hubKey, false) },
		"pending": func() { e.rcv.WelcomeStored("w1", "alice", e.hubKey, true) },
		"hubkey": func() {
			other := *e
			other.hubKey = bytes.Clone(e.hubKey)
			other.hubKey[0] ^= 0xff
			e.rcv.WelcomeStored("w1", "alice", other.hubKey, false)
		},
	} {
		l, c := e.link("w1", "alice", 10)
		e.rcv.LinkAdmitted(l)
		if _, err := e.rcv.Apply(l, e.frame(l, 1, docA)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		args()
		if e.onDisk() != nil {
			t.Fatalf("%s: the document stayed", name)
		}
		c()
	}
}

// The connector reports the stored welcome to the hook before it closes the
// link for the pending usage registration.
func TestConnectorReportsTheWelcomeBeforeTheRegistrationGateAndHubLink(t *testing.T) {
	f := newConnFixture(t)
	type seen struct {
		owner   string
		pending bool
	}
	var got atomic.Pointer[seen]
	f.conn = f.newConnector(func(c *ConnectorConfig) {
		c.UsageConnectionAllowed = f.st.UsageConnectionAllowed
		c.OnWelcomeStored = func(w HubWelcome, _ ed25519.PublicKey) { got.Store(&seen{owner: w.OwnerID, pending: w.Pending}) }
		c.Out = &logBuf{}
	})
	if f.conn.HubLink() != HubLinkDisconnected {
		t.Fatalf("hub link before connecting = %q", f.conn.HubLink())
	}
	var links atomic.Int32
	f.conn.OnLink("x", func(*Link) { links.Add(1) })
	w := okWelcome()
	w.OwnerID = "bob"
	w.UsagePolicy = ops.UsageVersion
	f.dial.script(f.welcomeStep(newFakeSession(), w), f.welcomeStep(newFakeSession(), w))
	f.run()
	f.waitState(ConnBackoff)
	if g := got.Load(); g == nil || g.owner != "bob" {
		t.Fatalf("the welcome hook did not run before the registration gate: %+v", g)
	}
	if links.Load() != 0 {
		t.Fatal("the link was served")
	}
	if f.conn.HubLink() != HubLinkRegistrationPending {
		t.Fatalf("hub link = %q, want registration_pending", f.conn.HubLink())
	}
	en, err := f.st.LoadEnrollment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.AcceptUsageRegistration(context.Background(), en.HubPub, en.WorkerID, en.Owner); err != nil {
		t.Fatal(err)
	}
	f.step(time.Minute)
	f.waitState(ConnConnected)
	if f.conn.HubLink() != HubLinkConnected {
		t.Fatalf("hub link = %q, want connected", f.conn.HubLink())
	}
}

// The certificate (or the link) ending while the commit waited for its locks:
// nothing becomes visible and no revision is recorded as applied.
func TestReadbackFenceEndingDuringTheWaitPublishesNothingAndRecordsNothing(t *testing.T) {
	e := newRBEnv(t)
	l, cancel := e.link("w1", "alice", 1)
	defer cancel()
	e.rcv.LinkAdmitted(l)
	e.rcv.SetGuard(func(ctx context.Context, _ uint64, live func() bool, fn func() error) error {
		cancel() // the fence ends while the receiver waits for the locks
		if !live() {
			return errGuardExpired
		}
		return fn()
	})
	if _, err := e.rcv.Apply(l, e.frame(l, 1, docA)); err == nil || proto.ReadbackReason(err) != proto.ReadbackFenced {
		t.Fatalf("err = %v, want a fenced refusal", err)
	}
	if e.onDisk() != nil || e.pub.Usage() != nil {
		t.Fatal("a document became visible after the fence ended")
	}
	if e.rcv.have {
		t.Fatal("a revision was recorded as applied")
	}

	// A live fence under the same guard publishes and records.
	l2, c2 := e.link("w1", "alice", 1)
	defer c2()
	e.rcv.LinkAdmitted(l2)
	e.rcv.SetGuard(func(_ context.Context, _ uint64, live func() bool, fn func() error) error {
		if !live() {
			return errGuardExpired
		}
		return fn()
	})
	ack, err := e.rcv.Apply(l2, e.frame(l2, 1, docA))
	mustAck(t, ack, err, 1)
}

// The fence ends during the write itself: the document is removed again and
// the revision is not recorded.
func TestReadbackFenceEndingDuringTheWriteRemovesTheDocument(t *testing.T) {
	e := newRBEnv(t)
	l, cancel := e.link("w1", "alice", 1)
	defer cancel()
	e.rcv.LinkAdmitted(l)
	e.rcv.SetGuard(func(_ context.Context, _ uint64, live func() bool, fn func() error) error {
		err := fn()
		cancel()
		return err
	})
	if _, err := e.rcv.Apply(l, e.frame(l, 1, docA)); err == nil {
		t.Fatal("acknowledged although the link ended during the write")
	}
	if e.onDisk() != nil || e.rcv.have {
		t.Fatal("the document stayed or was recorded")
	}
}

// The real enrollment guard: a stale generation or an ended certificate never
// runs the commit.
func TestEnrollmentGuardRunsTheCommitOnlyWhileTheFenceHolds(t *testing.T) {
	enr := NewEnrollments(nil, t.TempDir())
	ran := false
	fn := func() error { ran = true; return nil }
	if err := enr.WithEnrollmentIfLive(context.Background(), 7, nil, fn); !errors.Is(err, ErrEnrollmentChanged) || ran {
		t.Fatalf("stale generation: err %v, ran %v", err, ran)
	}
	if err := enr.WithEnrollmentIfLive(context.Background(), 0, func() bool { return false }, fn); !errors.Is(err, errGuardExpired) || ran {
		t.Fatalf("expired: err %v, ran %v", err, ran)
	}
	if err := enr.WithEnrollmentIfLive(context.Background(), 0, func() bool { return true }, fn); err != nil || !ran {
		t.Fatalf("live: err %v, ran %v", err, ran)
	}
}

// A clear frame removes the document, is bound and ordered like a document, and
// a document can follow it.
func TestReadbackClearFrame(t *testing.T) {
	e := newRBEnv(t)
	l, cancel := e.link("w1", "alice", 1)
	defer cancel()
	e.rcv.LinkAdmitted(l)
	if _, err := e.rcv.Apply(l, e.frame(l, 1, docA)); err != nil {
		t.Fatal(err)
	}
	clr := proto.ReadbackApply{Type: proto.TypeReadbackApply, Revision: 2, OwnerID: "alice", WorkerID: "w1", HubKey: e.hubFP, Clear: true}
	ack, err := e.rcv.Apply(l, clr)
	mustAck(t, ack, err, 2)
	if e.onDisk() != nil || e.pub.Usage() != nil {
		t.Fatal("the clear left the document")
	}
	if ack, err = e.rcv.Apply(l, clr); err != nil || ack.Revision != 2 { // a lost acknowledgement
		t.Fatalf("repeat of the clear = %+v, %v", ack, err)
	}
	stale := clr
	stale.Revision = 1
	if _, err := e.rcv.Apply(l, stale); err == nil {
		t.Fatal("a stale clear was accepted")
	}
	wrong := clr
	wrong.Revision, wrong.OwnerID = 3, "bob"
	if _, err := e.rcv.Apply(l, wrong); err == nil {
		t.Fatal("a clear built for another owner was accepted")
	}
	ack, err = e.rcv.Apply(l, e.frame(l, 3, docA))
	mustAck(t, ack, err, 3)
	if !bytes.Equal(e.onDisk(), []byte(docA)) {
		t.Fatal("no document after the clear")
	}
	bad := clr
	bad.Revision, bad.Doc = 4, json.RawMessage(docA)
	if bad.Check() == nil {
		t.Fatal("a clear with a document passes Check")
	}
}

// A removal that fails (a reader holds the file on Windows; here a directory
// that cannot be written) is retried with a bounded backoff, then recorded as a
// tombstone and done when the publisher attaches on the next start.
func TestReadbackFailedRemovalIsRetriedAndLeavesATombstone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	e := newRBEnv(t)
	tomb := t.TempDir()
	e.rcv.SetTombstoneDir(tomb)
	var sleeps []time.Duration
	e.rcv.sleep = func(d time.Duration) { sleeps = append(sleeps, d) }
	l, cancel := e.link("w1", "alice", 1)
	defer cancel()
	e.rcv.LinkAdmitted(l)
	if _, err := e.rcv.Apply(l, e.frame(l, 1, docA)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	e.rcv.Clear()
	_ = os.Chmod(e.dir, 0o700)
	if len(sleeps) != len(usageUnlinkBackoff) {
		t.Fatalf("retries = %v, want %d bounded backoffs", sleeps, len(usageUnlinkBackoff))
	}
	if e.onDisk() == nil {
		t.Skip("the file could be removed despite the read-only directory on this platform")
	}
	if _, err := os.Stat(filepath.Join(tomb, usageClearTombstone)); err != nil {
		t.Fatalf("no tombstone after a failed removal: %v", err)
	}
	// The next start: a fresh receiver with the same state directory.
	next := NewReadbackReceiver(nil, nil)
	next.SetTombstoneDir(tomb)
	next.SetPublisher(e.pub)
	if e.onDisk() != nil {
		t.Fatal("the pending removal was not done on the next start")
	}
	if _, err := os.Stat(filepath.Join(tomb, usageClearTombstone)); err == nil {
		t.Fatal("the tombstone stayed after the removal")
	}
}

// A wipe before any publisher exists (the daemon is still starting) cannot
// remove a file of a previous run; it is recorded and done at attach.
func TestReadbackClearBeforeThePublisherExistsIsDoneOnAttach(t *testing.T) {
	e := newRBEnv(t)
	l, cancel := e.link("w1", "alice", 1)
	defer cancel()
	e.rcv.LinkAdmitted(l)
	if _, err := e.rcv.Apply(l, e.frame(l, 1, docA)); err != nil {
		t.Fatal(err)
	}
	tomb := t.TempDir()
	r := NewReadbackReceiver(nil, nil)
	r.SetTombstoneDir(tomb)
	r.Clear()
	if e.onDisk() == nil {
		t.Fatal("nothing should be removed without a publisher")
	}
	r.SetPublisher(e.pub)
	if e.onDisk() != nil {
		t.Fatal("the recorded removal was not done")
	}
}

// The signed live revoke on the production op server: it reaches the
// connector's connection-bound verification and wipe, which removes the
// usage document on a real Worker before any reconnect.
func TestOpServerRoutesALiveRevokeToTheWipeAndClearsUsage(t *testing.T) {
	f := newConnFixture(t)
	e := newRBEnv(t)
	e.hubKey = f.hub.Public()
	fp, _ := identity.FingerprintOf(f.hub.Public())
	e.hubFP = fp.String()
	e.rcv = NewReadbackReceiver(nil, f.enr.Moved)
	e.rcv.SetGuard(f.enr.WithEnrollmentIfLive)
	e.rcv.SetPublisher(e.pub)
	f.enr.SetWipeHook(e.rcv.Clear)
	srv, err := NewOpServer(OpServerConfig{Executor: rbExec{}, Readback: e.rcv, Revoker: f.conn})
	if err != nil {
		t.Fatal(err)
	}
	live := newLiveHub(t)
	f.dial.script(func(p ConnectParams) (*Attempt, error) {
		w := okWelcome()
		w.OwnerID = "alice"
		return &Attempt{Pin: p.Pin, OpPub: p.Pin.Pub, TranscriptHash: live.hash, Version: proto.Current,
			Reply: Reply{Type: proto.TypeWelcome, Welcome: w}, Session: live.workerSess}, nil
	})
	f.conn.OnLink("op-server", srv.Serve)
	f.run()
	f.waitState(ConnConnected)

	apply := proto.ReadbackApply{Type: proto.TypeReadbackApply, Revision: 1, OwnerID: "alice", WorkerID: "w-1", HubKey: e.hubFP, Doc: json.RawMessage(docA)}
	eventually(t, func() bool {
		body := live.sendFrame(t, apply)
		var res struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(body, &res) == nil && res.Type == proto.TypeOpResult
	})
	if !bytes.Equal(e.onDisk(), []byte(docA)) {
		t.Fatal("the document was not published")
	}

	// An unsigned revoke is refused and changes nothing.
	body := live.sendFrame(t, map[string]any{"type": proto.TypeRevoke, "sig": identity.B64([]byte("nope"))})
	var ef proto.ErrorFrame
	if err := json.Unmarshal(body, &ef); err != nil || ef.Code != proto.CodeForbidden {
		t.Fatalf("answer to a bad revoke = %s", body)
	}
	if !bytes.Equal(e.onDisk(), []byte(docA)) {
		t.Fatal("a refused revoke removed the document")
	}

	body = live.sendFrame(t, revokeFor(t, f.hub, live.hash))
	var ack map[string]string
	if err := json.Unmarshal(body, &ack); err != nil || ack["type"] != proto.TypeOpResult || ack["op"] != "revoke" {
		t.Fatalf("answer = %s (%v), want the revoke ack", body, err)
	}
	f.waitIdle("not enrolled")
	if _, ok, _ := f.enr.Load(context.Background()); ok {
		t.Fatal("the enrollment was not wiped")
	}
	if e.onDisk() != nil || e.pub.Usage() != nil {
		t.Fatal("the revoked Worker still publishes its usage document")
	}
}

// A welcomed connection of a Worker the Hub has not approved refuses
// operations: it is not a usable Hub link. Solo has no Hub at all.
func TestHubLinkSaysPendingApprovalAndSolo(t *testing.T) {
	f := newConnFixture(t)
	w := okWelcome()
	w.Pending = true
	f.dial.script(f.welcomeStep(newFakeSession(), w))
	f.run()
	f.waitState(ConnConnected)
	if got := f.conn.HubLink(); got != HubLinkRegistrationPending {
		t.Fatalf("hub link while the approval is pending = %q, want registration_pending", got)
	}
	solo := newConnFixture(t, func(c *ConnectorConfig) { c.HubURL = "" })
	if got := solo.conn.HubLink(); got != HubLinkNone {
		t.Fatalf("hub link of a Solo Worker = %q, want none", got)
	}
}
