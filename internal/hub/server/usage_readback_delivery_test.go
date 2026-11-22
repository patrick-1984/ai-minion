package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

type fakeDir struct {
	mu    sync.Mutex
	owner map[string]string
	hook  func() // runs on every read, before it answers
}

func (f *fakeDir) WorkerOwner(_ context.Context, id string) (string, error) {
	if f.hook != nil {
		f.hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.owner[id]
	if !ok {
		return "", store.ErrWorkerNotFound
	}
	return o, nil
}

func (f *fakeDir) set(id, owner string) { f.mu.Lock(); f.owner[id] = owner; f.mu.Unlock() }

type fakeDoc struct {
	mu   sync.Mutex
	raw  map[string][]byte // by owner
	hash map[string]string
	n    int
}

func (f *fakeDoc) documentBytes(_ context.Context, owner string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return f.raw[owner], f.hash[owner], nil
}

func (f *fakeDoc) set(owner, raw, hash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raw[owner], f.hash[owner] = []byte(raw), hash
}

type fakeSend struct {
	mu     sync.Mutex
	frames []proto.ReadbackApply
	script func(n int, f proto.ReadbackApply) error // nil: ack
}

func (f *fakeSend) SendReadback(_ context.Context, _ workers.Online, fr proto.ReadbackApply) (proto.ReadbackAck, error) {
	f.mu.Lock()
	f.frames = append(f.frames, fr)
	n := len(f.frames)
	f.mu.Unlock()
	if f.script != nil {
		if err := f.script(n, fr); err != nil {
			return proto.ReadbackAck{}, err
		}
	}
	return proto.ReadbackAck{Revision: fr.Revision}, nil
}

func (f *fakeSend) got() []proto.ReadbackApply {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proto.ReadbackApply(nil), f.frames...)
}

type deliveryRig struct {
	t      *testing.T
	d      *readbackDelivery
	dir    *fakeDir
	doc    *fakeDoc
	send   *fakeSend
	online workers.Online
	done   chan struct{}
	stop   context.CancelFunc
}

type endedSession struct{ done chan struct{} }

func (endedSession) OpenOp(context.Context) (*transport.Stream, error) { return nil, errors.New("no") }
func (endedSession) Close() error                                      { return nil }
func (e endedSession) Done() <-chan struct{}                           { return e.done }

func newDeliveryRig(t *testing.T) *deliveryRig {
	t.Helper()
	r := &deliveryRig{t: t, dir: &fakeDir{owner: map[string]string{"w1": "alice"}},
		doc: &fakeDoc{raw: map[string][]byte{}, hash: map[string]string{}}, send: &fakeSend{}, done: make(chan struct{})}
	r.doc.set("alice", `{"schema":"minion.usage-meters/1","n":1}`, "h1")
	r.d = &readbackDelivery{log: slog.New(slog.DiscardHandler), dir: r.dir, doc: r.doc, send: r.send, now: time.Now,
		reconcile: time.Hour, refresh: time.Hour, retryMin: 5 * time.Millisecond, retryMax: 20 * time.Millisecond,
		slots: make(chan struct{}, 4), kick: make(chan struct{})}
	r.online = workers.Online{WorkerID: "w1", OwnerID: "alice", Features: []string{proto.FeatureUsageReadbackV1},
		TranscriptHash: make([]byte, 32), HubRoot: make([]byte, 32)}
	for i := range r.online.HubRoot {
		r.online.HubRoot[i] = byte(i + 1)
	}
	return r
}

func (r *deliveryRig) start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.stop = cancel
	finished := make(chan struct{})
	go func() { defer close(finished); r.d.serve(ctx, r.online, endedSession{done: r.done}) }()
	r.t.Cleanup(func() { cancel(); <-finished })
}

func rbWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func rbSettle() { time.Sleep(60 * time.Millisecond) }

func TestDeliverySendsOnAdmissionThenStaysQuietUntilTheContentChanges(t *testing.T) {
	r := newDeliveryRig(t)
	r.start()
	rbWait(t, "the first frame", func() bool { return len(r.send.got()) == 1 })
	f := r.send.got()[0]
	if f.Revision != 1 || f.OwnerID != "alice" || f.WorkerID != "w1" || f.HubKey == "" || string(f.Doc) != `{"schema":"minion.usage-meters/1","n":1}` {
		t.Fatalf("frame = %+v", f)
	}
	r.d.invalidate() // nothing changed: a rebuild finds the same content
	rbSettle()
	if n := len(r.send.got()); n != 1 {
		t.Fatalf("%d frames after an invalidation without a change", n)
	}
	r.doc.set("alice", `{"schema":"minion.usage-meters/1","n":2}`, "h2")
	r.d.invalidate()
	rbWait(t, "the changed document", func() bool { return len(r.send.got()) == 2 })
	if f := r.send.got()[1]; f.Revision != 2 || string(f.Doc) != `{"schema":"minion.usage-meters/1","n":2}` {
		t.Fatalf("second frame = %+v", f)
	}
}

func TestDeliveryReconcilesWithoutAnInvalidationAndRefreshesTheLease(t *testing.T) {
	r := newDeliveryRig(t)
	r.d.reconcile, r.d.refresh = 15*time.Millisecond, time.Hour
	r.start()
	rbWait(t, "the first frame", func() bool { return len(r.send.got()) == 1 })
	r.doc.set("alice", `{"schema":"minion.usage-meters/1","n":9}`, "h9") // a change that no operation announced
	rbWait(t, "the reconciliation to deliver it", func() bool { return len(r.send.got()) == 2 })

	r2 := newDeliveryRig(t)
	r2.d.reconcile, r2.d.refresh = 15*time.Millisecond, 30*time.Millisecond
	r2.start()
	rbWait(t, "a refresh of the unchanged document", func() bool { return len(r2.send.got()) >= 3 })
	fr := r2.send.got()
	if fr[1].Revision != 2 || fr[2].Revision != 3 {
		t.Fatalf("a refresh is a new revision: %d %d", fr[1].Revision, fr[2].Revision)
	}
}

func TestDeliveryRetriesALostAckWithTheSameRevisionAndBytes(t *testing.T) {
	r := newDeliveryRig(t)
	r.send.script = func(n int, _ proto.ReadbackApply) error {
		if n <= 2 {
			return errors.New("the answer was lost") // the Worker may have applied it
		}
		return nil
	}
	r.start()
	rbWait(t, "three attempts", func() bool { return len(r.send.got()) >= 3 })
	fr := r.send.got()
	for _, f := range fr[:3] {
		if f.Revision != 1 || string(f.Doc) != string(fr[0].Doc) {
			t.Fatalf("a retry changed the revision or the bytes: %+v", fr)
		}
	}
	rbSettle()
	if n := len(r.send.got()); n != 3 {
		t.Fatalf("%d frames after the acknowledgement; want no more", n)
	}
}

func TestDeliveryContinuesAboveARevisionTheWorkerHoldsAlready(t *testing.T) {
	r := newDeliveryRig(t)
	r.send.script = func(n int, f proto.ReadbackApply) error {
		if f.Revision < 4 {
			return proto.ReadbackRefusal(proto.CodeConflict, proto.ReadbackStale, "stale")
		}
		return nil
	}
	r.start()
	rbWait(t, "revision 4 to be acknowledged", func() bool {
		fr := r.send.got()
		return len(fr) >= 4 && fr[len(fr)-1].Revision == 4
	})
}

func TestDeliveryStopsWhenTheWorkerRefusesTheBindingOrTheOwnerChanged(t *testing.T) {
	r := newDeliveryRig(t)
	r.send.script = func(int, proto.ReadbackApply) error {
		return proto.ReadbackRefusal(proto.CodeConflict, proto.ReadbackFenced, "another owner")
	}
	r.start()
	rbWait(t, "the first attempt", func() bool { return len(r.send.got()) == 1 })
	rbSettle()
	if n := len(r.send.got()); n != 1 {
		t.Fatalf("the Hub hammered a Worker that refused the binding: %d frames", n)
	}

	// A Worker that cannot publish: nothing to retry on this link.
	r3 := newDeliveryRig(t)
	r3.send.script = func(int, proto.ReadbackApply) error { return proto.NewError(proto.CodeUnsupportedOp, "no") }
	r3.start()
	rbWait(t, "the attempt", func() bool { return len(r3.send.got()) == 1 })
	rbSettle()
	if n := len(r3.send.got()); n != 1 {
		t.Fatalf("%d attempts to a Worker that cannot publish", n)
	}

	// Reassignment before the first delivery: the connection belongs to Alice, the store says Bob.
	r2 := newDeliveryRig(t)
	r2.dir.set("w1", "bob")
	r2.start()
	rbSettle()
	if n := len(r2.send.got()); n != 0 {
		t.Fatalf("a frame built for the previous owner was sent: %+v", r2.send.got())
	}
}

// The reassignment commits while Alice's document is being built: the recheck
// right before the write finds Bob and nothing is sent.
func TestDeliveryOwnershipTransferRaceSendsNothingForTheOldOwner(t *testing.T) {
	r := newDeliveryRig(t)
	reads := 0
	r.dir.hook = func() {
		reads++
		if reads == 2 { // the read after the document was built
			r.dir.mu.Lock()
			r.dir.owner["w1"] = "bob"
			r.dir.mu.Unlock()
		}
	}
	r.start()
	rbSettle()
	if n := len(r.send.got()); n != 0 {
		t.Fatalf("a delayed Alice update was dispatched after the transfer: %+v", r.send.got())
	}
}

func TestDeliveryEndsWithTheConnection(t *testing.T) {
	r := newDeliveryRig(t)
	finished := make(chan struct{})
	go func() { r.d.serve(context.Background(), r.online, endedSession{done: r.done}); close(finished) }()
	rbWait(t, "the first frame", func() bool { return len(r.send.got()) == 1 })
	close(r.done)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop outlived its connection")
	}
}

func TestDeliveryEligibilityFollowsA72Admission(t *testing.T) {
	feat := []string{proto.FeatureUsageReadbackV1}
	cases := []struct {
		name string
		w    workers.Online
		want bool
	}{
		{"approved, accepted", workers.Online{Features: feat, Facts: transport.WorkerFacts{UsageConsent: "accepted"}}, true},
		{"approved legacy without acceptance", workers.Online{Features: feat, Facts: transport.WorkerFacts{UsageConsent: "legacy_pending"}}, true},
		{"approved, old Worker without consent facts", workers.Online{Features: feat}, true},
		// The Worker closes such a link before it runs any handler; the hello
		// fact alone (stale on a first connection) is not a reason to hold back.
		{"required but unaccepted: the Worker itself refuses the link", workers.Online{Features: feat, Facts: transport.WorkerFacts{UsageConsent: "pending"}}, true},
		{"pending approval", workers.Online{Features: feat, Pending: true}, false},
		{"old Worker without the feature", workers.Online{Facts: transport.WorkerFacts{UsageConsent: "accepted"}}, false},
	}
	for _, c := range cases {
		if got := readbackEligible(c.w); got != c.want {
			t.Errorf("%s: eligible = %v, want %v", c.name, got, c.want)
		}
	}
}

// The collector's slots and failure state are not the delivery's: with every
// collector slot taken the document is still delivered at once.
func TestDeliveryIsIndependentOfTheUsageCollector(t *testing.T) {
	collector := &usageCollector{slots: make(chan struct{}, 8)}
	for i := 0; i < cap(collector.slots); i++ {
		collector.slots <- struct{}{}
	}
	r := newDeliveryRig(t)
	r.start()
	rbWait(t, "the delivery while the collector is saturated", func() bool { return len(r.send.got()) == 1 })
	if len(collector.slots) != cap(collector.slots) {
		t.Fatal("the delivery touched the collector's slots")
	}
}
