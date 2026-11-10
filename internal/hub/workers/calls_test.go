package workers

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// admitted dials as an enrolled Worker, reads the welcome and returns the
// Worker side of the yamux session.
func (h *testHub) admitted(key *identity.Key) *transport.Session {
	h.t.Helper()
	res, err := h.dial(h.workerConfig(key, "send_text"))
	if err != nil {
		h.t.Fatal(err)
	}
	decodeWelcome(h.t, res)
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// call sends one op.request on a new call stream and returns the answer frame.
func callOnce(t *testing.T, sess *transport.Session, rq map[string]any) (typ string, body json.RawMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sess.OpenCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Reset()
	_ = st.SetDeadline(time.Now().Add(10 * time.Second))
	if err := st.WriteFrame(rq); err != nil {
		t.Fatal(err)
	}
	body, err = st.ReadFrame()
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	typ, err = proto.FrameType(body)
	if err != nil {
		t.Fatal(err)
	}
	return typ, body
}

func retireRequest() map[string]any {
	id := ops.NewUUIDv7()
	return map[string]any{
		"type": proto.TypeOpRequest, "op": ops.OpWorkerRetire, "request_id": id,
		"args": map[string]any{"request_id": id, "force": false},
	}
}

// The old key of a replaced Worker identity is rejected by the real Hub after
// worker_retire: the enrollment is gone, a tombstone remains, the Worker is no
// longer online, and the next connect with the old key gets a revoke that the
// Hub key signed over that connection.
func TestWorkerRetireRemovesTheEnrollmentAndTheOldKeyGetsASignedRevoke(t *testing.T) {
	h := newTestHub(t, nil)
	oldKey, otherKey := newKey(t), newKey(t)
	w := h.enrol(oldKey, "owner-1", "pi")
	h.enrol(otherKey, "owner-1", "other")

	sess := h.admitted(oldKey)
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })

	typ, body := callOnce(t, sess, retireRequest())
	if typ != proto.TypeOpResult {
		t.Fatalf("answer = %s", body)
	}
	var res struct {
		Result ops.WriteResult `json:"result"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.Result.Outcome != ops.OutcomeDelivered {
		t.Fatalf("result = %s (%v)", body, err)
	}

	if _, err := h.st.WorkerByPub(context.Background(), oldKey.Public()); err == nil {
		t.Error("the enrollment of the old key still exists")
	}
	tomb, err := h.st.TombstoneByPub(context.Background(), oldKey.Public())
	if err != nil || tomb.WorkerID != w.ID || tomb.RemovedBy != "worker:"+w.ID {
		t.Fatalf("tombstone = %+v err=%v", tomb, err)
	}
	if _, err := h.st.WorkerByPub(context.Background(), otherKey.Public()); err != nil {
		t.Errorf("another Worker was affected: %v", err)
	}
	_ = sess.Close()
	eventually(t, "the retired Worker to go offline", func() bool { return !h.svc.Registry().IsOnline(w.ID) })

	// The next connect with the old key: a signed revoke, no welcome.
	res2, err := h.dial(h.workerConfig(oldKey, "send_text"))
	if err != nil {
		t.Fatal(err)
	}
	m := readControl(t, res2)
	if got := frameType(t, m); got != proto.TypeRevoke {
		t.Fatalf("frame type after the retirement = %q, want revoke", got)
	}
	raw, _ := json.Marshal(m)
	var f RevokeFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRevoke(res2.HubPub, res2.TranscriptHash[:], f); err != nil {
		t.Errorf("revoke signature: %v", err)
	}
	if h.svc.Registry().IsOnline(w.ID) {
		t.Error("the old key is online after the retirement")
	}

	page, err := h.audit.List(context.Background(), audit.Filter{Action: ops.OpWorkerRetire})
	if err != nil || len(page.Events) != 1 || page.Events[0].Target != w.ID || page.Events[0].Actor != "worker:"+w.ID {
		t.Errorf("audit = %+v err=%v", page, err)
	}
}

// A retirement repeated after the Worker row is gone (a duplicate or an admin
// removal in between) still succeeds: the goal is reached.
func TestWorkerRetireIsIdempotentWhenTheRowIsAlreadyGone(t *testing.T) {
	h := newTestHub(t, nil)
	// Conn.Welcome registers the Worker as online BEFORE the mandatory admission
	// recheck (connect.go), and that recheck legitimately ends a session whose
	// enrollment is already gone. Tombstoning right after IsOnline therefore
	// races with it and can close the session before the retirement is called.
	// The test waits for the recheck of this connection to finish, so that the
	// tombstone is the only change the retirement meets.
	rechecked := make(chan struct{}, 4)
	h.svc.Handler().afterRecheck = func() { rechecked <- struct{}{} }
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	sess := h.admitted(key)
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	select {
	case <-rechecked:
	case <-timeout(t):
		t.Fatal("the admission recheck did not finish")
	}
	if err := h.st.TombstoneWorker(context.Background(), w.ID, "admin", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	typ, body := callOnce(t, sess, retireRequest())
	if typ != proto.TypeOpResult {
		t.Fatalf("answer = %s", body)
	}
}

func TestCallStreamRefusesWhatItDoesNotServe(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	sess := h.admitted(key)
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })

	expect := func(name string, rq map[string]any, want proto.Code) {
		t.Helper()
		typ, body := callOnce(t, sess, rq)
		var f proto.ErrorFrame
		if typ != proto.TypeError || json.Unmarshal(body, &f) != nil || f.Code != want {
			t.Errorf("%s: answer = %s, want error %s", name, body, want)
		}
	}
	expect("unknown op", map[string]any{"type": proto.TypeOpRequest, "op": "sessions_list"}, proto.CodeUnsupportedOp)
	expect("not an op.request", map[string]any{"type": "ping"}, proto.CodeInvalidArgument)
	expect("bad request id", map[string]any{"type": proto.TypeOpRequest, "op": ops.OpWorkerRetire,
		"args": map[string]any{"request_id": "nope"}}, proto.CodeInvalidArgument)
	expect("no args", map[string]any{"type": proto.TypeOpRequest, "op": ops.OpWorkerRetire}, proto.CodeInvalidArgument)

	// None of those removed the Worker.
	if _, err := h.st.WorkerByPub(context.Background(), key.Public()); err != nil {
		t.Errorf("the Worker was removed by a refused call: %v", err)
	}
	if !h.svc.Registry().IsOnline(w.ID) {
		t.Error("the Worker went offline")
	}
}

// A Hub without a removal function answers unsupported_op and keeps the Worker.
func TestWorkerRetireNeedsARemovalFunction(t *testing.T) {
	h := newTestHub(t, nil)
	h.svc.SetRemoveWorker(nil, false)
	key := newKey(t)
	h.enrol(key, "owner-1", "pi")
	sess := h.admitted(key)
	typ, body := callOnce(t, sess, retireRequest())
	var f proto.ErrorFrame
	if typ != proto.TypeError || json.Unmarshal(body, &f) != nil || f.Code != proto.CodeUnsupportedOp {
		t.Fatalf("answer = %s", body)
	}
}

func TestWorkerRetireFailureIsAnInternalErrorAndKeepsTheEnrollment(t *testing.T) {
	boom := errors.New("disk full")
	h := newTestHub(t, func(o *ServiceOptions) {
		o.RemoveWorker = func(context.Context, string, string, int64) error { return boom }
	})
	key := newKey(t)
	h.enrol(key, "owner-1", "pi")
	sess := h.admitted(key)
	typ, body := callOnce(t, sess, retireRequest())
	var f proto.ErrorFrame
	if typ != proto.TypeError || json.Unmarshal(body, &f) != nil || f.Code != proto.CodeInternal {
		t.Fatalf("answer = %s", body)
	}
	if _, err := h.st.WorkerByPub(context.Background(), key.Public()); err != nil {
		t.Errorf("enrollment lost: %v", err)
	}
}

// An event stream is not served yet; it must not wedge the connection.
func TestEventStreamsAreResetAndCallsStillWork(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	h.enrol(key, "owner-1", "pi")
	sess := h.admitted(key)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ev, err := sess.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ev.Reset()
	if typ, body := callOnce(t, sess, retireRequest()); typ != proto.TypeOpResult {
		t.Fatalf("answer = %s", body)
	}
}

// A removal that writes its own audit record leaves the call with nothing to
// record: there is no second, best-effort worker_retire record.
func TestWorkerRetireWritesNoSecondAuditRecordWhenTheRemovalAudits(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	h.svc.SetRemoveWorker(func(ctx context.Context, id, by string, at int64) error {
		return h.st.TombstoneWorker(ctx, id, by, at)
	}, true)
	sess := h.admitted(key)
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	if typ, body := callOnce(t, sess, retireRequest()); typ != proto.TypeOpResult {
		t.Fatalf("answer = %s", body)
	}
	page, err := h.audit.List(context.Background(), audit.Filter{Action: ops.OpWorkerRetire})
	if err != nil || len(page.Events) != 0 {
		t.Errorf("audit = %+v err=%v, want no worker_retire record", page, err)
	}
}

// A second connection with the old key that is admitted before the removal
// commits replaces the retiring session in the registry. The retirement must
// end that connection too: after it, nothing is online as the retired Worker.
func TestWorkerRetireEndsAReplacementConnectionThatRacedTheRemoval(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")

	var mu sync.Mutex
	var replacement *transport.WorkerResult
	var raceErr error
	// The first connect is the Worker's own; the second is the racing one.
	rechecked := make(chan struct{}, 4)
	var connects atomic.Int32
	h.svc.Handler().afterRecheck = func() {
		if connects.Add(1) == 2 {
			rechecked <- struct{}{}
		}
	}
	h.svc.SetRemoveWorker(func(ctx context.Context, id, by string, at int64) error {
		// The enrollment still exists here: the reconnect is admitted, takes
		// the registry entry of the retiring session and passes the recheck
		// before the removal commits.
		res, err := h.dial(h.workerConfig(key, "send_text"))
		mu.Lock()
		replacement, raceErr = res, err
		mu.Unlock()
		if err == nil {
			decodeWelcome(t, res)
			select {
			case <-rechecked:
			case <-time.After(5 * time.Second):
			}
		}
		return h.st.TombstoneWorker(context.Background(), id, by, at)
	}, false)

	sess := h.admitted(key)
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	// The replacement ends the retiring session, so the answer may never
	// arrive; the removal itself still commits.
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		st, err := sess.OpenCall(ctx)
		if err != nil {
			return
		}
		defer st.Reset()
		_ = st.SetDeadline(time.Now().Add(10 * time.Second))
		if st.WriteFrame(retireRequest()) == nil {
			_, _ = st.ReadFrame()
		}
	}()
	mu.Lock()
	res, raceErr := replacement, raceErr
	mu.Unlock()
	if raceErr != nil {
		t.Fatalf("the racing reconnect: %v", raceErr)
	}
	if res == nil {
		t.Fatal("the racing reconnect did not happen")
	}
	t.Cleanup(func() { _ = res.Channel.Close() })
	eventually(t, "the replacement connection to be ended", func() bool { return !h.svc.Registry().IsOnline(w.ID) })
	time.Sleep(retireGrace + 500*time.Millisecond)
	if h.svc.Registry().IsOnline(w.ID) {
		t.Error("a connection with the retired key is online after the grace")
	}
}
