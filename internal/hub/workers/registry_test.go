package workers

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// fakeSession is an OpSession that records what the registry does to it.
type fakeSession struct {
	mu      sync.Mutex
	opens   int
	openErr error
	closed  bool
	done    chan struct{}
}

func newFakeSession() *fakeSession { return &fakeSession{done: make(chan struct{})} }

func (f *fakeSession) OpenOp(context.Context) (*transport.Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	return nil, f.openErr
}

func (f *fakeSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.done)
	}
	return nil
}

func (f *fakeSession) Done() <-chan struct{} { return f.done }

func (f *fakeSession) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeSession) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func info(id string, ops ...string) Online {
	return Online{WorkerID: id, Name: id + "-name", OwnerID: "u1", Kind: "private", Version: proto.Version{Major: 1}, Ops: ops}
}

func TestRegistryOnlineAndOfflineTransitions(t *testing.T) {
	r := NewRegistry()
	if r.IsOnline("w1") || r.Len() != 0 {
		t.Fatal("a new registry has online Workers")
	}
	s := newFakeSession()
	r.Add(info("w1", "sessions_list", "send_text"), s)
	got, ok := r.Get("w1")
	if !ok || !r.IsOnline("w1") || r.Len() != 1 {
		t.Fatal("the Worker is not online after Add")
	}
	if got.Name != "w1-name" || got.OwnerID != "u1" || got.Kind != "private" || got.Version != (proto.Version{Major: 1}) || got.ConnectedAt.IsZero() {
		t.Errorf("online record = %+v", got)
	}
	if !reflect.DeepEqual(got.Ops, []string{"send_text", "sessions_list"}) {
		t.Errorf("ops = %v, want them sorted", got.Ops)
	}
	// The session ending takes the Worker offline by itself.
	_ = s.Close()
	eventually(t, "the Worker to go offline", func() bool { return !r.IsOnline("w1") })
	if r.Len() != 0 || len(r.List()) != 0 {
		t.Error("an ended session is still listed")
	}
}

func TestRegistryGetReturnsASnapshot(t *testing.T) {
	r := NewRegistry()
	r.Add(info("w1", "send_text"), newFakeSession())
	got, _ := r.Get("w1")
	got.Ops[0] = "tampered"
	again, _ := r.Get("w1")
	if again.Ops[0] != "send_text" {
		t.Error("a caller changed the registry through a snapshot")
	}
}

func TestRegistryListIsOrdered(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"w3", "w1", "w2"} {
		r.Add(info(id), newFakeSession())
	}
	var ids []string
	for _, o := range r.List() {
		ids = append(ids, o.WorkerID)
	}
	if !reflect.DeepEqual(ids, []string{"w1", "w2", "w3"}) {
		t.Errorf("ids = %v", ids)
	}
}

func TestReconnectReplacesTheOldSession(t *testing.T) {
	r := NewRegistry()
	old, fresh := newFakeSession(), newFakeSession()
	r.Add(info("w1", "send_text"), old)
	r.Add(info("w1", "send_text", "answer"), fresh)
	if !old.isClosed() {
		t.Error("the replaced session was not closed")
	}
	if fresh.isClosed() {
		t.Error("the new session was closed")
	}
	// The old session ending must not take the new connection offline.
	time.Sleep(50 * time.Millisecond)
	got, ok := r.Get("w1")
	if !ok || !got.Announces("answer") || r.Len() != 1 {
		t.Fatalf("the new connection was lost: %+v ok=%v", got, ok)
	}
	_ = fresh.Close()
	eventually(t, "offline after the new session ends", func() bool { return !r.IsOnline("w1") })
}

func TestOpenOpRefusesWhatTheWorkerDidNotAnnounce(t *testing.T) {
	r := NewRegistry()
	s := newFakeSession()
	r.Add(info("w1", "sessions_list"), s)
	_, err := r.OpenOp(context.Background(), "w1", "send_text")
	if !errors.Is(err, proto.NewError(proto.CodeUnsupportedOp, "")) {
		t.Fatalf("err = %v, want unsupported_op", err)
	}
	if s.openCount() != 0 {
		t.Error("a stream was opened for an op the Worker did not announce")
	}
	if _, err := r.OpenOp(context.Background(), "w1", ""); proto.CodeOf(err) != proto.CodeUnsupportedOp {
		t.Errorf("empty op: %v", err)
	}
}

func TestOpenOpOfflineWorker(t *testing.T) {
	r := NewRegistry()
	_, err := r.OpenOp(context.Background(), "nobody", "send_text")
	if proto.CodeOf(err) != proto.CodeWorkerOffline {
		t.Fatalf("err = %v, want worker_offline", err)
	}
	// A Worker whose session just ended is offline, not an internal error.
	s := newFakeSession()
	s.openErr = errors.New("session closed")
	r.Add(info("w1", "send_text"), s)
	_ = s.Close()
	_, err = r.OpenOp(context.Background(), "w1", "send_text")
	if code := proto.CodeOf(err); code != proto.CodeWorkerOffline {
		t.Fatalf("err = %v (%s), want worker_offline", err, code)
	}
}

func TestOpenOpReportsOpenFailure(t *testing.T) {
	r := NewRegistry()
	s := newFakeSession()
	s.openErr = errors.New("too many streams")
	r.Add(info("w1", "send_text"), s)
	_, err := r.OpenOp(context.Background(), "w1", "send_text")
	if err == nil || proto.CodeOf(err) == proto.CodeWorkerOffline {
		t.Fatalf("err = %v", err)
	}
	if s.openCount() != 1 {
		t.Errorf("opens = %d", s.openCount())
	}
}

func TestAnnouncedOpsAreNormalised(t *testing.T) {
	long := make([]byte, maxOpNameLen+1)
	for i := range long {
		long[i] = 'a'
	}
	many := make([]string, 0, 400)
	for i := 0; i < 400; i++ {
		many = append(many, "op_"+string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676)))
	}
	cases := map[string]struct {
		in   []string
		want []string
	}{
		"duplicates":   {[]string{"b", "a", "b"}, []string{"a", "b"}},
		"malformed":    {[]string{"Send_Text", "send-text", "", "send text", "ok_1", "_x", "9a"}, []string{"ok_1"}},
		"too long":     {[]string{string(long), "short"}, []string{"short"}},
		"none":         {nil, []string{}},
		"unknown kept": {[]string{"future_op"}, []string{"future_op"}},
	}
	for name, c := range cases {
		if got := normaliseOps(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
	if got := normaliseOps(many); len(got) != maxAnnouncedOps {
		t.Errorf("announced ops are not capped: %d", len(got))
	}
}

func TestDisconnectAndCloseAll(t *testing.T) {
	r := NewRegistry()
	a, b := newFakeSession(), newFakeSession()
	r.Add(info("a"), a)
	r.Add(info("b"), b)
	if r.Disconnect("nobody") {
		t.Error("Disconnect of an offline Worker reported true")
	}
	if !r.Disconnect("a") || !a.isClosed() {
		t.Error("Disconnect did not close the session")
	}
	eventually(t, "a offline", func() bool { return !r.IsOnline("a") })
	r.CloseAll()
	if !b.isClosed() {
		t.Error("CloseAll left a session open")
	}
	eventually(t, "registry empty", func() bool { return r.Len() == 0 })
}

func TestRegistryIsRaceFree(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s := newFakeSession()
				r.Add(info("w", "send_text"), s)
				_, _ = r.Get("w")
				_ = r.List()
				_, _ = r.OpenOp(context.Background(), "w", "send_text")
				if j%3 == 0 {
					_ = s.Close()
				}
			}
		}()
	}
	wg.Wait()
	r.CloseAll()
	eventually(t, "registry empty", func() bool { return r.Len() == 0 })
}
