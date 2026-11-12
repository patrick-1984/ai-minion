package server

import (
	"context"
	"errors"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// TestRetireAndLiveEventsWorkTogetherInTheComposedHub is the regression of the
// integration of Stories 4.7 and 5.3: the composed Hub (real store, workers
// service, API, route, removal and events modules) serves, on the one real
// session of a Worker, its event stream and its worker_retire call. Every round
// is a new identity of the Worker (a re-enrollment) over a real handshake; the
// full removal of Story 4.10 is the one that serves the retirement.
func TestRetireAndLiveEventsWorkTogetherInTheComposedHub(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "retire-events-integration-pw-1")
	r := NewRegistry()
	for _, err := range []error{
		registerCore(r),
		registerStore(r),
		r.Register(settingsModuleName, settingsModuleOrder, initSettingsModule,
			DeclaresService[*SettingsService](), DeclaresService[config.PanelSource]()),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	registerAuditModule(r)
	registerAuthz(r)
	registerAuth(r)
	registerHubIDModule(r)
	registerWorkersModule(r)
	registerAPI(r, ops.Default)
	registerAPIKeyModule(r)
	registerWorkersRemoveModule(r)
	registerRouteModule(r)
	registerEventsModule(r)
	var (
		st     *store.Store
		wsv    *workers.Service
		events *workers.EventHub
	)
	if err := r.Register("grab", eventsModuleOrder+5, func(_ context.Context, env *Env) error {
		var err error
		if st, err = Require[*store.Store](env); err != nil {
			return err
		}
		if wsv, err = Require[*workers.Service](env); err != nil {
			return err
		}
		events, err = Require[*workers.EventHub](env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h := startHub(t, &Config{DataDir: hubstore.Dir(t)}, r)
	ctx := context.Background()
	admin, err := st.UserByIdentifier(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}

	for round := 0; round < 10; round++ {
		key, err := identity.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		w := store.WorkerRow{ID: store.NewID(), Pub: key.Public(), Name: "lab", OwnerID: admin.ID,
			Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}
		if err := st.InsertWorker(ctx, nil, w); err != nil {
			t.Fatal(err)
		}
		conn, err := transport.Dial(ctx, "https://"+h.srv.Addr())
		if err != nil {
			t.Fatal(err)
		}
		res, err := transport.WorkerHandshake(conn, transport.WorkerConfig{
			Key: key, Name: "lab", Ops: []string{"send_text"}, Mode: transport.ModeConnect,
			Pin: identity.Pin{Pub: wsv.Identity().Public()}, Timeout: 10 * time.Second,
		})
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if _, err := proto.ReadFrame(res.Channel, proto.StreamFrameLimit); err != nil { // welcome
			t.Fatal(err)
		}
		sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
		if err != nil {
			t.Fatal(err)
		}

		// Live events through the Hub's ingest.
		evCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ev, err := sess.OpenEvents(evCtx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if err := ev.WriteFrame(map[string]any{"type": proto.TypeEventsSnap, "seq": 0, "sessions": []map[string]any{
			{"name": "minion-1", "kind": "agent:claude", "state": "stable", "task": "build"}}}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the ingested session state", func() bool {
			s, ok := events.Sessions(w.ID)
			return ok && s.Online && len(s.Sessions) == 1
		})
		if err := ev.WriteFrame(map[string]any{"type": proto.TypeEvent, "seq": 1, "event": "state_changed",
			"session": "minion-1", "kind": "agent:claude", "state": "busy", "task": "build"}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "state_changed", func() bool {
			s, _ := events.Sessions(w.ID)
			return len(s.Sessions) == 1 && s.Sessions[0].State == "busy"
		})

		// worker_retire on the same session, while the event stream is open.
		cst, err := sess.OpenCall(ctx)
		if err != nil {
			t.Fatal(err)
		}
		id := ops.NewUUIDv7()
		_ = cst.SetDeadline(time.Now().Add(10 * time.Second))
		if err := cst.WriteFrame(map[string]any{"type": proto.TypeOpRequest, "op": ops.OpWorkerRetire,
			"request_id": id, "args": map[string]any{"request_id": id, "force": false}}); err != nil {
			t.Fatal(err)
		}
		body, err := cst.ReadFrame()
		if err != nil {
			t.Fatalf("round %d: worker_retire: %v", round, err)
		}
		if typ, _ := proto.FrameType(body); typ != proto.TypeOpResult {
			t.Fatalf("round %d: worker_retire answered %s", round, body)
		}
		if _, err := st.WorkerByID(ctx, w.ID); !errors.Is(err, store.ErrWorkerNotFound) {
			t.Errorf("round %d: the Worker row survived: %v", round, err)
		}
		if tomb, err := st.TombstoneByPub(ctx, key.Public()); err != nil || tomb.WorkerID != w.ID || tomb.RemovedBy != "worker:"+w.ID {
			t.Errorf("round %d: tombstone = %+v, %v", round, tomb, err)
		}
		_ = sess.Close()
		waitFor(t, "the Worker to go offline", func() bool {
			s, _ := events.Sessions(w.ID)
			return !s.Online && !wsv.Registry().IsOnline(w.ID)
		})
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
