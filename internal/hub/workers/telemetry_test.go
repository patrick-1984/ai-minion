package workers

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

type recTelemetry struct {
	mu        sync.Mutex
	conns     int
	auth      []string
	closed    []bool
	taps      []string
	frames    int
	routed    []string
	admission []string

	writesAfterAuth int
	served          []string
}

func (r *recTelemetry) FrameTap(workerID, _ string) transport.FrameTap {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.taps = append(r.taps, workerID)
	return r
}
func (r *recTelemetry) TapFrame(transport.TapFrame) { r.mu.Lock(); r.frames++; r.mu.Unlock() }
func (r *recTelemetry) Conn(c net.Conn) (net.Conn, ConnTelemetry) {
	r.mu.Lock()
	r.conns++
	r.mu.Unlock()
	return &orderConn{Conn: c, r: r}, recConn{r}
}
func (r *recTelemetry) OpRouted(workerID, op, outcome string, _ time.Duration) {
	r.mu.Lock()
	r.routed = append(r.routed, workerID+" "+op+" "+outcome)
	r.mu.Unlock()
}
func (r *recTelemetry) CallServed(_, op, outcome string) {
	r.mu.Lock()
	r.served = append(r.served, op+" "+outcome)
	r.mu.Unlock()
}
func (r *recTelemetry) Admission(lane string, ok bool) {
	r.mu.Lock()
	r.admission = append(r.admission, lane)
	r.mu.Unlock()
}
func (r *recTelemetry) TurnIngest(string) {}

// orderConn counts the writes that follow the report of the Worker.
type orderConn struct {
	net.Conn
	r *recTelemetry
}

func (c *orderConn) Write(b []byte) (int, error) {
	c.r.mu.Lock()
	if len(c.r.auth) > 0 {
		c.r.writesAfterAuth++
	}
	c.r.mu.Unlock()
	return c.Conn.Write(b)
}

type recConn struct{ r *recTelemetry }

func (c recConn) Authenticated(id string) {
	c.r.mu.Lock()
	c.r.auth = append(c.r.auth, id)
	c.r.mu.Unlock()
}
func (c recConn) Closed(ok bool) { c.r.mu.Lock(); c.r.closed = append(c.r.closed, ok); c.r.mu.Unlock() }

// The connect handler and the Router report to the installed telemetry: the
// accepted connection, its Worker, the frame tap of its session and every
// routed op with its outcome.
func TestTelemetryHooks(t *testing.T) {
	h := newTestHub(t, nil)
	rec := &recTelemetry{}
	h.svc.SetTelemetry(rec)
	w := connectScripted(t, h, idAlice, "tel-1", nil, ops.OpSessionsList)
	az := authz.New(fakeUsers{idAlice: {Role: authz.RoleUser, Active: true}}, h.svc.OwnerLookup())
	rt, err := NewRouter(RouterConfig{Backend: RegistryBackend(h.svc.Registry()), Directory: StoreDirectory{Store: h.st}, Authz: az,
		Telemetry: h.svc.Telemetry})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = rt.Call(context.Background(), CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: w.id})
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.conns != 1 || len(rec.auth) != 1 || rec.auth[0] != w.id || len(rec.taps) != 1 || rec.taps[0] != w.id {
		t.Fatalf("conns %d auth %v taps %v", rec.conns, rec.auth, rec.taps)
	}
	if len(rec.routed) != 1 || rec.routed[0][:len(w.id)] != w.id {
		t.Fatalf("routed %v", rec.routed)
	}
}

// The welcome bytes are counted for the authenticated Worker: the Worker is
// reported before the welcome is written.
func TestWelcomeIsAttributedToTheWorker(t *testing.T) {
	h := newTestHub(t, nil)
	rec := &recTelemetry{}
	h.svc.SetTelemetry(rec)
	_ = connectScripted(t, h, idAlice, "tel-w", nil, ops.OpSessionsList)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.writesAfterAuth == 0 {
		t.Fatal("the welcome was written before the Worker was reported")
	}
}

// A call refused before dispatch (a made-up Worker id, say) is not an op
// sent to a Worker: it is not counted and takes no Worker slot.
func TestRoutedMetricIgnoresRefusals(t *testing.T) {
	h := newTestHub(t, nil)
	rec := &recTelemetry{}
	h.svc.SetTelemetry(rec)
	az := authz.New(fakeUsers{idAlice: {Role: authz.RoleUser, Active: true}}, h.svc.OwnerLookup())
	rt, err := NewRouter(RouterConfig{Backend: RegistryBackend(h.svc.Registry()), Directory: StoreDirectory{Store: h.st}, Authz: az,
		Telemetry: h.svc.Telemetry})
	if err != nil {
		t.Fatal(err)
	}
	const fake = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	_, _ = rt.Call(context.Background(), CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: fake})
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.routed) != 0 {
		t.Fatalf("routed %q", rec.routed)
	}
}

// The sent op keeps its Worker id even when the registry no longer knows the
// connection when the call ends (a disconnect).
type goneBackend struct{ Backend }

func (goneBackend) Get(string) (Online, bool) { return Online{}, false }

func TestRoutedMetricKeepsTheWorkerAfterDisconnect(t *testing.T) {
	h := newTestHub(t, nil)
	rec := &recTelemetry{}
	h.svc.SetTelemetry(rec)
	w := connectScripted(t, h, idAlice, "tel-gone", nil, ops.OpSessionsList)
	az := authz.New(fakeUsers{idAlice: {Role: authz.RoleUser, Active: true}}, h.svc.OwnerLookup())
	rt, err := NewRouter(RouterConfig{Backend: goneBackend{RegistryBackend(h.svc.Registry())}, Directory: StoreDirectory{Store: h.st}, Authz: az,
		Telemetry: h.svc.Telemetry})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = rt.Call(context.Background(), CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: w.id})
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.routed) != 1 || !strings.HasPrefix(rec.routed[0], w.id+" ") {
		t.Fatalf("routed %q", rec.routed)
	}
}

type switchHandler struct {
	stuck   *atomic.Bool
	release chan struct{}
}

func (switchHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h switchHandler) Handle(context.Context, slog.Record) error {
	if h.stuck.Load() {
		<-h.release
	}
	return nil
}
func (h switchHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h switchHandler) WithGroup(string) slog.Handler      { return h }

// With the local output stalled (its lock held by the traffic writer), the
// Hub's close still returns: the offline and timeout records are best effort.
func TestServiceCloseWithStalledLocalOutput(t *testing.T) {
	stuck := &atomic.Bool{}
	rel := make(chan struct{})
	defer close(rel)
	h := newTestHub(t, func(o *ServiceOptions) { o.Logger = slog.New(switchHandler{stuck, rel}) })
	_ = connectScripted(t, h, idAlice, "stall-1", nil, ops.OpSessionsList)
	stuck.Store(true)
	done := make(chan struct{})
	go func() { h.svc.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("Close hung on a stalled log output")
	}
}

// Reproduce the CI race deterministically: the peer sees registration and
// shuts down just as the still-running connection handler logs "worker online".
type onlineStallHandler struct {
	entered chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (onlineStallHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h onlineStallHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "worker online" {
		h.once.Do(func() { close(h.entered) })
		<-h.release
	}
	return nil
}
func (h onlineStallHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h onlineStallHandler) WithGroup(string) slog.Handler      { return h }

func TestServiceCloseDuringOnlineLog(t *testing.T) {
	l := onlineStallHandler{entered: make(chan struct{}), release: make(chan struct{}), once: &sync.Once{}}
	defer close(l.release)
	h := newTestHub(t, func(o *ServiceOptions) { o.Logger = slog.New(l) })
	_ = connectScripted(t, h, idAlice, "online-log-stalled", nil, ops.OpSessionsList)
	select {
	case <-l.entered:
	case <-time.After(time.Second):
		t.Fatal("online log was not reached")
	}
	done := make(chan struct{})
	go func() { h.svc.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited for a stalled online log")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.svc.handler.Wait(ctx); err != nil {
		t.Fatalf("connection handler survived Close: %v", err)
	}
}
