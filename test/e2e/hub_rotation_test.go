//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/transport"
	wagent "github.com/patrick-1984/ai-minion/internal/worker/agent"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Story 4.9 integration test: the real Hub and the real Worker connector. A
// paired Worker is stopped, the Hub rotates its identity twice (once with a Hub
// restart in between), and the restarted Worker follows the transition chain
// without any human action. Run with "make e2e".

// persistentWorker is a Worker whose state survives a connector restart.
type persistentWorker struct {
	t    *testing.T
	hub  *connHub
	enr  *wagent.Enrollments
	st   *wstore.Store
	key  *identity.Key
	logs *syncLog
	stop func()
	conn *wagent.Connector
}

func newPersistentWorker(t *testing.T, h *connHub) *persistentWorker {
	t.Helper()
	dir := t.TempDir()
	st, err := wstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enr := wagent.NewEnrollments(st, dir)
	key, err := enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := enr.Commit(context.Background(), wagent.Enrollment{Pin: h.pin()}); err != nil {
		t.Fatal(err)
	}
	return &persistentWorker{t: t, hub: h, enr: enr, st: st, key: key, logs: &syncLog{}}
}

func (w *persistentWorker) start() {
	w.t.Helper()
	conn, err := wagent.NewConnector(wagent.ConnectorConfig{
		HubURL:     w.hub.url(),
		Name:       "pi",
		Ops:        func() []string { return []string{"sessions_list"} },
		Key:        w.enr.LoadKey,
		Enrollment: w.enr,
		Policy:     wagent.NewPolicy(w.st),
		Dialer:     wagent.NewHubDialer(),
		Backoff:    &transport.Backoff{Initial: 5 * time.Millisecond, Max: 40 * time.Millisecond},
		IdlePoll:   20 * time.Millisecond,
		Logger:     slog.New(slog.NewTextHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); conn.Run(ctx) }()
	w.conn = conn
	w.stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			w.t.Error("the connector did not stop")
		}
	}
	w.t.Cleanup(func() { w.stop() })
}

func TestWorkerOfflineThroughTwoHubRotationsReconnectsWithoutHumanAction(t *testing.T) {
	h := newConnHub(t)
	w := newPersistentWorker(t, h)
	row := h.enrol(w.key.Public(), "pi")
	originalPin := h.pin()

	w.start()
	eventually(t, "the Worker to be online", func() bool { return h.service().Registry().IsOnline(row.ID) })
	w.stop()
	h.service().Registry().Disconnect(row.ID)
	eventually(t, "the Worker to be offline", func() bool { return !h.service().Registry().IsOnline(row.ID) })

	ctx := context.Background()
	first, err := h.service().Rotator().Rotate(ctx, workers.RotateActor{ID: "admin-1"})
	if err != nil {
		t.Fatal(err)
	}
	// The Hub restarts between the rotations: the chain comes from the database.
	h.mu.Lock()
	addr := h.addr
	h.mu.Unlock()
	h.stop()
	h.start(addr)
	second, err := h.service().Rotator().Rotate(ctx, workers.RotateActor{ID: "admin-1"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 || second.Seq != 2 || first.NewFingerprint == second.NewFingerprint {
		t.Fatalf("rotations = %+v, %+v", first, second)
	}
	current := h.service().Identity().Public()
	if bytes.Equal(current, originalPin.Pub) {
		t.Fatal("the Hub key did not change")
	}
	// Nothing was touched on the Worker meanwhile.
	if enr, _, _ := w.enr.Load(ctx); !bytes.Equal(enr.Pin.Pub, originalPin.Pub) || enr.Pin.Seq != 0 {
		t.Fatalf("the Worker pin changed while it was stopped: %+v", enr.Pin)
	}

	w.start()
	eventually(t, "the Worker to be online at the rotated Hub", func() bool { return h.service().Registry().IsOnline(row.ID) })
	// The Hub counts the Worker online as soon as the handshake is done; the
	// Worker rewrites its pin just after and applies the welcome after that (two
	// separate writes under the enrollment generation, Story 4.7), so wait for
	// both before the assertions below.
	eventually(t, "the Worker to store the new pin and the welcome", func() bool {
		enr, ok, _ := w.enr.Load(ctx)
		return ok && enr.Pin.Seq == 2 && enr.WorkerID == row.ID
	})
	enr, ok, err := w.enr.Load(ctx)
	if err != nil || !ok {
		t.Fatalf("enrollment = %v, %v", ok, err)
	}
	if enr.Pin.Seq != 2 || !bytes.Equal(enr.Pin.Pub, current) {
		t.Errorf("the Worker pin = seq %d, want the current Hub key at seq 2", enr.Pin.Seq)
	}
	if enr.WorkerID != row.ID {
		t.Errorf("the pairing changed: %+v", enr)
	}
	eventually(t, "the pin advance to be logged", func() bool {
		return strings.Contains(w.logs.String(), "hub key pin advanced along the transition chain")
	})
}
