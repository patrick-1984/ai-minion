package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

type fakeElig struct {
	mu  sync.Mutex
	ok  bool
	err error
}

func (f *fakeElig) ReadbackRecipientEligible(context.Context, string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "alice", f.ok, f.err
}

func (f *fakeElig) set(ok bool) { f.mu.Lock(); f.ok = ok; f.mu.Unlock() }

// A recipient whose owner lost access gets no document; the Worker is told to
// clear what it holds, once; it gets documents again when it is eligible.
func TestDeliveryWithdrawsFromAnIneligibleRecipientAndResumesWhenEligible(t *testing.T) {
	r := newDeliveryRig(t)
	el := &fakeElig{ok: true}
	r.d.elig = el
	r.start()
	rbWait(t, "the first document", func() bool { return len(r.send.got()) == 1 })

	el.set(false)
	r.d.invalidate()
	rbWait(t, "the clear", func() bool { return len(r.send.got()) == 2 })
	clr := r.send.got()[1]
	if !clr.Clear || len(clr.Doc) != 0 || clr.Revision != 2 || clr.OwnerID != "alice" {
		t.Fatalf("clear frame = %+v", clr)
	}
	// More changes while ineligible: no document, no second clear.
	r.doc.set("alice", `{"schema":"minion.usage-meters/1","n":2}`, "h2")
	r.d.invalidate()
	rbSettle()
	if n := len(r.send.got()); n != 2 {
		t.Fatalf("%d frames while ineligible; want the document and one clear", n)
	}
	el.set(true)
	r.d.invalidate()
	rbWait(t, "the document after re-eligibility", func() bool { return len(r.send.got()) == 3 })
	if f := r.send.got()[2]; f.Clear || f.Revision != 3 || len(f.Doc) == 0 {
		t.Fatalf("resumed frame = %+v", f)
	}
}

// Eligibility is read again right before the write: a deactivation that commits
// while the document is built sends the withdrawal, never the document.
func TestDeliveryDeactivationWhileBuildingSendsNoDocument(t *testing.T) {
	r := newDeliveryRig(t)
	el := &fakeElig{ok: true}
	r.d.elig = el
	reads := 0
	r.dir.hook = func() {
		reads++
		if reads == 2 { // after the build
			el.set(false)
		}
	}
	r.start()
	rbWait(t, "the clear", func() bool { return len(r.send.got()) >= 1 })
	rbSettle()
	for _, f := range r.send.got() {
		if !f.Clear {
			t.Fatalf("a document was sent to an ineligible recipient: %+v", f)
		}
	}
}

func TestDeliveryEligibilityErrorSendsNothing(t *testing.T) {
	r := newDeliveryRig(t)
	r.d.elig = &fakeElig{ok: true, err: errors.New("db down")}
	r.start()
	rbSettle()
	if n := len(r.send.got()); n != 0 {
		t.Fatalf("%d frames although eligibility could not be read", n)
	}
}

// An unrelated usage edit wakes every loop; a loop that waits to retry a failed
// revision keeps its deadline unless the content for its Worker changed.
func TestDeliveryUnrelatedEditsDoNotBypassTheRetryBackoff(t *testing.T) {
	r := newDeliveryRig(t)
	r.d.retryMin, r.d.retryMax = 400*time.Millisecond, 400*time.Millisecond
	r.send.script = func(n int, _ proto.ReadbackApply) error {
		if n == 1 {
			return errors.New("the presence file is unavailable")
		}
		return nil
	}
	r.start()
	rbWait(t, "the failed attempt", func() bool { return len(r.send.got()) == 1 })
	for range 10 {
		r.d.invalidate() // an unrelated edit: this Worker's document is unchanged
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(r.send.got()); n != 1 {
		t.Fatalf("%d frames: unrelated edits retried the failed revision before its deadline", n)
	}
	rbWait(t, "the retry at its deadline", func() bool { return len(r.send.got()) == 2 })
	if a, b := r.send.got()[0], r.send.got()[1]; a.Revision != b.Revision || string(a.Doc) != string(b.Doc) {
		t.Fatalf("the retry changed the revision or the bytes")
	}

	// Changed content is sent at once, even while a retry waits.
	r2 := newDeliveryRig(t)
	r2.d.retryMin, r2.d.retryMax = 5*time.Second, 5*time.Second
	var fail atomic.Bool
	fail.Store(true)
	r2.send.script = func(int, proto.ReadbackApply) error {
		if fail.Load() {
			return errors.New("lost")
		}
		return nil
	}
	r2.start()
	rbWait(t, "the failed attempt", func() bool { return len(r2.send.got()) == 1 })
	fail.Store(false)
	r2.doc.set("alice", `{"schema":"minion.usage-meters/1","n":2}`, "h2")
	r2.d.invalidate()
	rbWait(t, "the changed document", func() bool { return len(r2.send.got()) == 2 })
	if r2.send.got()[1].Revision != 2 {
		t.Fatalf("frames = %+v", r2.send.got())
	}
}

type closeSession struct {
	done   chan struct{}
	closed atomic.Bool
}

func (*closeSession) OpenOp(context.Context) (*transport.Stream, error) { return nil, errors.New("no") }
func (c *closeSession) Close() error                                    { c.closed.Store(true); return nil }
func (c *closeSession) Done() <-chan struct{}                           { return c.done }

var _ workers.OpSession = (*closeSession)(nil)

// A delivery that finds another owner in the store than its connection was
// welcomed for ends that exact connection, so that the reconnect is welcomed
// with the new owner.
func TestDeliveryEndsTheStaleConnectionOnAnOwnerMismatch(t *testing.T) {
	r := newDeliveryRig(t)
	r.dir.set("w1", "bob")
	sess := &closeSession{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() { defer close(finished); r.d.serve(ctx, r.online, sess) }()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("the loop did not end")
	}
	if !sess.closed.Load() {
		t.Fatal("the stale connection was not closed")
	}
	if n := len(r.send.got()); n != 0 {
		t.Fatalf("%d frames for the stale owner", n)
	}
}
