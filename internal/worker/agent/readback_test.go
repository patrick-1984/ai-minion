//go:build !windows

package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/presence"
)

const testInstance = "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f"

type rbEnv struct {
	t        *testing.T
	pub      *presence.Publisher
	dir      string
	rcv      *ReadbackReceiver
	hubKey   ed25519.PublicKey
	hubFP    string
	movedGen map[uint64]bool
}

func newRBEnv(t *testing.T) *rbEnv {
	t.Helper()
	state := t.TempDir()
	layout := presence.Layout{Scope: presence.ScopeUser, UID: os.Geteuid(), StateDir: state, PrivateDir: filepath.Join(state, "presence")}
	pub, err := presence.New(presence.Options{Layout: layout, Instance: testInstance, Version: "0.0.0-test", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pub.Stop() })
	hub, _, _ := ed25519.GenerateKey(rand.Reader)
	fp, _ := identity.FingerprintOf(hub)
	e := &rbEnv{t: t, pub: pub, dir: layout.PrivateDir, hubKey: hub, hubFP: fp.String(), movedGen: map[uint64]bool{}}
	e.rcv = NewReadbackReceiver(nil, func(g uint64) bool { return e.movedGen[g] })
	e.rcv.SetPublisher(pub)
	return e
}

func (e *rbEnv) link(worker, owner string, gen uint64) (*Link, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	return &Link{Welcome: HubWelcome{WorkerID: worker, OwnerID: owner}, ctx: ctx, gen: gen, pin: identity.Pin{Pub: e.hubKey}}, cancel
}

func (e *rbEnv) frame(l *Link, rev uint64, doc string) proto.ReadbackApply {
	return proto.ReadbackApply{Type: proto.TypeReadbackApply, Revision: rev, OwnerID: l.Welcome.OwnerID, WorkerID: l.Welcome.WorkerID, HubKey: e.hubFP, Doc: json.RawMessage(doc)}
}

func (e *rbEnv) onDisk() []byte {
	b, _ := os.ReadFile(filepath.Join(e.dir, testInstance+".usage.json"))
	return b
}

const docA = `{"schema":"minion.usage-meters/1","generated_at":1,"slots":[]}`
const docB = `{"schema":"minion.usage-meters/1","generated_at":2,"slots":[]}`

func mustAck(t *testing.T, ack proto.ReadbackAck, err *proto.Error, rev uint64) {
	t.Helper()
	if err != nil || ack.Revision != rev {
		t.Fatalf("ack = %+v, err = %v; want revision %d", ack, err, rev)
	}
}

func TestReadbackPublishesVerbatimAndAcknowledgesTheRevision(t *testing.T) {
	e := newRBEnv(t)
	l, cancel := e.link("w1", "alice", 1)
	defer cancel()
	e.rcv.LinkAdmitted(l)
	ack, err := e.rcv.Apply(l, e.frame(l, 1, docA))
	mustAck(t, ack, err, 1)
	if got := e.onDisk(); !bytes.Equal(got, []byte(docA)) || !bytes.Equal(e.pub.Usage(), []byte(docA)) {
		t.Fatalf("published %q, want the bytes the Hub sent", got)
	}
}

func TestReadbackRefusesEveryMismatchedBindingAndPublishesNothing(t *testing.T) {
	e := newRBEnv(t)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	ofp, _ := identity.FingerprintOf(other)
	cases := map[string]func(l *Link, f *proto.ReadbackApply, cancel context.CancelFunc){
		"another owner":    func(_ *Link, f *proto.ReadbackApply, _ context.CancelFunc) { f.OwnerID = "bob" },
		"another worker":   func(_ *Link, f *proto.ReadbackApply, _ context.CancelFunc) { f.WorkerID = "w2" },
		"another hub key":  func(_ *Link, f *proto.ReadbackApply, _ context.CancelFunc) { f.HubKey = ofp.String() },
		"pending approval": func(l *Link, _ *proto.ReadbackApply, _ context.CancelFunc) { l.Welcome.Pending = true },
		"ended link":       func(_ *Link, _ *proto.ReadbackApply, cancel context.CancelFunc) { cancel() },
		"expired Hub certificate": func(l *Link, _ *proto.ReadbackApply, _ context.CancelFunc) {
			g := &expiryGuard{exp: transport.Expiry{WallMs: 1, DeadlineMono: 0}, clk: transport.SystemWallMono, log: slog.New(slog.DiscardHandler)}
			l.ctx = withGuard(l.ctx, g)
		},
		"moved enrollment": func(l *Link, _ *proto.ReadbackApply, _ context.CancelFunc) { e.movedGen[l.gen] = true },
		"no owner binding": func(l *Link, f *proto.ReadbackApply, _ context.CancelFunc) { l.Welcome.OwnerID, f.OwnerID = "", "x" },
		"missing hub key":  func(_ *Link, f *proto.ReadbackApply, _ context.CancelFunc) { f.HubKey = "" },
		"zero revision":    func(_ *Link, f *proto.ReadbackApply, _ context.CancelFunc) { f.Revision = 0 },
		"oversized document": func(_ *Link, f *proto.ReadbackApply, _ context.CancelFunc) {
			f.Doc = json.RawMessage(`{"schema":"minion.usage-meters/1","pad":"` + string(bytes.Repeat([]byte("x"), proto.MaxReadbackDocBytes)) + `"}`)
		},
		"another schema": func(_ *Link, f *proto.ReadbackApply, _ context.CancelFunc) {
			f.Doc = json.RawMessage(`{"schema":"other/1"}`)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e.movedGen = map[uint64]bool{}
			e.rcv.Clear()
			l, cancel := e.link("w1", "alice", 7)
			defer cancel()
			e.rcv.LinkAdmitted(l)
			f := e.frame(l, 1, docA)
			mutate(l, &f, cancel)
			if _, err := e.rcv.Apply(l, f); err == nil {
				t.Fatal("the frame was accepted")
			}
			if e.onDisk() != nil || e.pub.Usage() != nil {
				t.Fatal("a refused frame published something")
			}
		})
	}
}

func TestReadbackFrameForANonCurrentLinkIsRefused(t *testing.T) {
	e := newRBEnv(t)
	old, c1 := e.link("w1", "alice", 1)
	defer c1()
	e.rcv.LinkAdmitted(old)
	cur, c2 := e.link("w1", "alice", 1)
	defer c2()
	e.rcv.LinkAdmitted(cur)
	if _, err := e.rcv.Apply(old, e.frame(old, 1, docA)); err == nil {
		t.Fatal("a frame of an older link was accepted")
	}
	if _, err := e.rcv.Apply(nil, e.frame(cur, 1, docA)); err == nil {
		t.Fatal("a frame without a link was accepted")
	}
}

func TestReadbackRejectsStaleAndReorderedRevisionsAndRepeatsALostAck(t *testing.T) {
	e := newRBEnv(t)
	l, cancel := e.link("w1", "alice", 1)
	defer cancel()
	e.rcv.LinkAdmitted(l)
	ack, err := e.rcv.Apply(l, e.frame(l, 2, docA))
	mustAck(t, ack, err, 2)
	// The Hub did not get the acknowledgement and sends revision 2 again.
	ack, err = e.rcv.Apply(l, e.frame(l, 2, docA))
	mustAck(t, ack, err, 2)
	// A delayed older revision arrives after a newer one.
	_, err = e.rcv.Apply(l, e.frame(l, 1, docB))
	if err == nil || proto.ReadbackReason(err) != proto.ReadbackStale {
		t.Fatalf("revision 1 after 2: %v", err)
	}
	// The same revision with another document is not a repeat.
	_, err = e.rcv.Apply(l, e.frame(l, 2, docB))
	if err == nil || proto.ReadbackReason(err) != proto.ReadbackStale {
		t.Fatalf("revision 2 with another document: %v", err)
	}
	if !bytes.Equal(e.onDisk(), []byte(docA)) {
		t.Fatalf("a refused revision changed the published document: %s", e.onDisk())
	}
	ack, err = e.rcv.Apply(l, e.frame(l, 3, docB))
	mustAck(t, ack, err, 3)
	if !bytes.Equal(e.onDisk(), []byte(docB)) {
		t.Fatal("revision 3 was not published")
	}
}

// A delayed update built for Alice must never publish once Bob's connection
// exists, and the document Alice's connection published goes with her link.
func TestReadbackOwnershipTransferRace(t *testing.T) {
	e := newRBEnv(t)
	alice, cancelA := e.link("w1", "alice", 1)
	defer cancelA()
	e.rcv.LinkAdmitted(alice)
	ack, err := e.rcv.Apply(alice, e.frame(alice, 1, docA))
	mustAck(t, ack, err, 1)

	// Reassignment: the Worker reconnects as Bob's.
	cancelA()
	bob, cancelB := e.link("w1", "bob", 2)
	defer cancelB()
	e.rcv.LinkAdmitted(bob)
	if e.onDisk() != nil || e.pub.Usage() != nil {
		t.Fatal("Alice's document stayed readable after the Worker became Bob's")
	}
	// The delayed update of Alice's connection, and one of Alice's owner id on Bob's.
	if _, err := e.rcv.Apply(alice, e.frame(alice, 2, docA)); err == nil {
		t.Fatal("a delayed update of Alice's link was accepted")
	}
	if _, err := e.rcv.Apply(bob, e.frame(alice, 2, docA)); err == nil {
		t.Fatal("an update built for Alice was accepted on Bob's link")
	}
	if e.onDisk() != nil {
		t.Fatal("something was published")
	}
	ack, err = e.rcv.Apply(bob, e.frame(bob, 1, docB))
	mustAck(t, ack, err, 1)
	if !bytes.Equal(e.onDisk(), []byte(docB)) {
		t.Fatal("Bob's document was not published")
	}
}

func TestReadbackSameBindingReconnectKeepsTheDocumentAndRestartsRevisions(t *testing.T) {
	e := newRBEnv(t)
	l1, c1 := e.link("w1", "alice", 1)
	defer c1()
	e.rcv.LinkAdmitted(l1)
	_, err := e.rcv.Apply(l1, e.frame(l1, 5, docA))
	if err != nil {
		t.Fatal(err)
	}
	c1()
	l2, c2 := e.link("w1", "alice", 1)
	defer c2()
	e.rcv.LinkAdmitted(l2)
	if !bytes.Equal(e.onDisk(), []byte(docA)) {
		t.Fatal("a reconnect of the same owner dropped the document")
	}
	ack, err2 := e.rcv.Apply(l2, e.frame(l2, 1, docB))
	mustAck(t, ack, err2, 1)
}

func TestReadbackClearOnWipeAndPinChange(t *testing.T) {
	e := newRBEnv(t)
	l, cancel := e.link("w1", "alice", 1)
	defer cancel()
	e.rcv.LinkAdmitted(l)
	if _, err := e.rcv.Apply(l, e.frame(l, 1, docA)); err != nil {
		t.Fatal(err)
	}
	e.rcv.Clear() // revoke, reset, new identity
	if e.onDisk() != nil || e.pub.Usage() != nil {
		t.Fatal("the wipe left the document")
	}
	// The same link, whose enrollment the wipe moved, can publish nothing more.
	e.movedGen[1] = true
	if _, err := e.rcv.Apply(l, e.frame(l, 2, docA)); err == nil {
		t.Fatal("a frame was accepted after the enrollment moved")
	}
	// A new link under another Hub key (pin change) clears what the old pin published.
	e.movedGen = map[uint64]bool{}
	l2, c2 := e.link("w1", "alice", 2)
	defer c2()
	e.rcv.LinkAdmitted(l2)
	if _, err := e.rcv.Apply(l2, e.frame(l2, 1, docA)); err != nil {
		t.Fatal(err)
	}
	newHub, _, _ := ed25519.GenerateKey(rand.Reader)
	l3, c3 := e.link("w1", "alice", 3)
	defer c3()
	l3.pin = identity.Pin{Pub: newHub}
	e.rcv.LinkAdmitted(l3)
	if e.onDisk() != nil {
		t.Fatal("a document published under the old Hub key stayed after the pin changed")
	}
}

func TestReadbackSurvivesAWipeThatRacesThePublication(t *testing.T) {
	e := newRBEnv(t)
	l, cancel := e.link("w1", "alice", 1)
	defer cancel()
	e.rcv.LinkAdmitted(l)
	// The wipe moves the generation after the first check and before the write
	// is looked at again: the document must not stay.
	calls := 0
	e.rcv.moved = func(uint64) bool { calls++; return calls > 1 }
	if _, err := e.rcv.Apply(l, e.frame(l, 1, docA)); err == nil {
		t.Fatal("the frame was acknowledged although the enrollment moved during the write")
	}
	if e.onDisk() != nil || e.pub.Usage() != nil {
		t.Fatal("the document stayed")
	}
}

func TestOpServerServesTheReadbackFrameAndNothingElseReachesIt(t *testing.T) {
	e := newRBEnv(t)
	srv, err := NewOpServer(OpServerConfig{Executor: rbExec{}, Readback: e.rcv})
	if err != nil {
		t.Fatal(err)
	}
	l, cancel := e.link("w1", "alice", 1)
	defer cancel()
	e.rcv.LinkAdmitted(l)
	ctx := withLink(l.Context(), l)

	st := newFakeOpStream(t, e.frame(l, 1, docA))
	srv.ServeStream(ctx, st)
	if got := st.types(t); len(got) != 1 || got[0] != proto.TypeOpResult {
		t.Fatalf("frames = %v", got)
	}
	var res struct {
		Result proto.ReadbackAck `json:"result"`
	}
	if err := json.Unmarshal(st.frame(0), &res); err != nil || res.Result.Revision != 1 {
		t.Fatalf("answer = %s", st.frame(0))
	}
	if !bytes.Equal(e.onDisk(), []byte(docA)) {
		t.Fatal("the acknowledged document is not published")
	}

	// A stale frame is answered with an error frame carrying the reason.
	st = newFakeOpStream(t, e.frame(l, 1, docB))
	srv.ServeStream(ctx, st)
	ef := st.errorFrame(t)
	if ef.Code != proto.CodeConflict || proto.ReadbackReason(ef.Err()) != proto.ReadbackStale {
		t.Fatalf("error frame = %+v", ef)
	}

	// A server without a receiver refuses it; a context without the link too.
	bare, _ := NewOpServer(OpServerConfig{Executor: rbExec{}})
	st = newFakeOpStream(t, e.frame(l, 2, docB))
	bare.ServeStream(ctx, st)
	if st.errorFrame(t).Code != proto.CodeUnsupportedOp {
		t.Fatal("a Worker without a receiver accepted the frame")
	}
	st = newFakeOpStream(t, e.frame(l, 2, docB))
	srv.ServeStream(context.Background(), st)
	if st.errorFrame(t).Code != proto.CodeUnsupportedOp {
		t.Fatal("a frame without a link was served")
	}
}

// The frame is not an op: it is in no catalog, an op.request cannot name it,
// and the executor behind the local socket, MCP and the Worker calls does not
// know it.
func TestReadbackIsNotAnOpAndTheExecutorRefusesIt(t *testing.T) {
	for _, name := range []string{proto.TypeReadbackApply, "readback_apply", "worker_readback", "usage_readback_apply", "readback"} {
		if op, ok := ops.Lookup(name); ok && op.Scope == ops.ScopeWorker {
			t.Fatalf("%q is a Worker op of the catalog", name)
		}
	}
	e := newRBEnv(t)
	srv, _ := NewOpServer(OpServerConfig{Executor: rbExec{}, Readback: e.rcv})
	l, cancel := e.link("w1", "alice", 1)
	defer cancel()
	e.rcv.LinkAdmitted(l)
	ctx := withLink(l.Context(), l)
	for _, name := range []string{proto.TypeReadbackApply, "worker_readback", "readback"} {
		st := newFakeOpStream(t, OpRequestFrame{Type: proto.TypeOpRequest, Op: name, Actions: []ops.Action{ops.ActionRead, ops.ActionOperate}, Args: json.RawMessage(`{"revision":1}`)})
		srv.ServeStream(ctx, st)
		if st.errorFrame(t).Code == "" {
			t.Fatalf("an op.request named %q was served: %v", name, st.types(t))
		}
	}
	if e.onDisk() != nil {
		t.Fatal("an op.request published a document")
	}
}

// rbExec is the executor behind the local socket and the Hub's op streams as
// far as these tests go: it knows no op.
type rbExec struct{}

func (rbExec) Execute(context.Context, Request) (json.RawMessage, error) {
	return nil, proto.NewError(proto.CodeUnsupportedOp, "unknown op")
}

func TestReadbackReadinessBeforeAndWithoutAPublisher(t *testing.T) {
	e := newRBEnv(t)
	l, cancel := e.link("w1", "alice", 1)
	defer cancel()
	late := NewReadbackReceiver(nil, nil) // the connector dialed before the presence module started
	late.LinkAdmitted(l)
	if !late.Ready() {
		t.Fatal("the feature is withdrawn before it is known that the Worker cannot publish")
	}
	_, err := late.Apply(l, e.frame(l, 1, docA))
	if err == nil || err.Code != proto.CodeBusy {
		t.Fatalf("a frame before the publisher exists: %v (want busy, so that the Hub retries)", err)
	}
	late.SetUnavailable()
	if late.Ready() {
		t.Fatal("the feature is announced by a Worker whose presence record is unavailable")
	}
	if _, err := late.Apply(l, e.frame(l, 1, docA)); err == nil || err.Code != proto.CodeUnsupportedOp {
		t.Fatalf("a Worker without a publisher: %v", err)
	}
	late.SetPublisher(e.pub)
	if !late.Ready() {
		t.Fatal("not ready with a publisher")
	}
}

// The frame as the Hub writes it and the Worker reads it: a clear travels
// without a document, an old Hub's "doc":null is read as no document, and the
// Worker removes the file and acknowledges.
func TestReadbackClearSurvivesTheWire(t *testing.T) {
	for name, wire := range map[string]func(proto.ReadbackApply) []byte{
		"hub encoding": func(f proto.ReadbackApply) []byte { b, _ := json.Marshal(f); return b },
		"doc null": func(f proto.ReadbackApply) []byte {
			b, _ := json.Marshal(f)
			return append(b[:len(b)-1], []byte(`,"doc":null}`)...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newRBEnv(t)
			l, cancel := e.link("w1", "alice", 1)
			defer cancel()
			e.rcv.LinkAdmitted(l)
			ack, err := e.rcv.Apply(l, e.frame(l, 1, docA))
			mustAck(t, ack, err, 1)
			clr := e.frame(l, 2, "")
			clr.Doc, clr.Clear = nil, true
			var got proto.ReadbackApply
			if jerr := json.Unmarshal(wire(clr), &got); jerr != nil {
				t.Fatal(jerr)
			}
			ack, err = e.rcv.Apply(l, got)
			mustAck(t, ack, err, 2)
			if e.onDisk() != nil {
				t.Fatal("the clear left the document")
			}
		})
	}
	if b, _ := json.Marshal(proto.ReadbackApply{Type: "t", Clear: true}); strings.Contains(string(b), `"doc"`) {
		t.Fatalf("a clear carries a doc member: %s", b)
	}
}

// A closed transport session ends the publication even while the connector has
// not cancelled the link's context yet.
func TestReadbackClosedTransportPublishesNothing(t *testing.T) {
	e := newRBEnv(t)
	l, cancel := e.link("w1", "alice", 1)
	defer cancel()
	sess := newFakeSession()
	l.Session = sess
	e.rcv.LinkAdmitted(l)
	sess.drop(errors.New("transport closed"))
	if _, err := e.rcv.Apply(l, e.frame(l, 1, docA)); err == nil {
		t.Fatal("a document was acknowledged on a closed transport")
	}
	if e.onDisk() != nil {
		t.Fatal("a document was published on a closed transport")
	}
}
