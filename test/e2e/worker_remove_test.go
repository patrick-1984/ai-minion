//go:build e2e

package e2e

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/transport"
	wagent "github.com/patrick-1984/ai-minion/internal/worker/agent"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Story 4.10 integration tests: the real workers.Remover with the real
// RegistryRevoker on the Hub side, the real agent.Connector on the Worker
// side, over a real TLS WebSocket. The three cases of the test plan: remove
// while online (wipe observed), remove while the connection has no revoke
// handler (wipe on the reconnect), remove while offline then connect (wipe on
// connect).

// removalWorker is a Worker with a persistent state directory whose connector
// can be stopped and started again, as a restart of the service does.
type removalWorker struct {
	t    *testing.T
	dir  string
	st   *wstore.Store
	enr  *wagent.Enrollments
	key  *identity.Key
	logs *syncLog

	conn   *wagent.Connector
	cancel context.CancelFunc
	done   chan struct{}
}

func newRemovalWorker(t *testing.T, h *connHub) *removalWorker {
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
	return &removalWorker{t: t, dir: dir, st: st, enr: enr, key: key, logs: &syncLog{}}
}

// start runs a new connector; serveRevokes installs the live-revoke consumer.
func (w *removalWorker) start(h *connHub, serveRevokes bool) {
	w.t.Helper()
	conn, err := wagent.NewConnector(wagent.ConnectorConfig{
		HubURL:     h.url(),
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
	if serveRevokes {
		conn.OnLink("revoke", conn.ServeRevokes)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); conn.Run(ctx) }()
	w.conn, w.cancel, w.done = conn, cancel, done
	w.t.Cleanup(w.stop)
}

func (w *removalWorker) stop() {
	if w.cancel == nil {
		return
	}
	w.cancel()
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		w.t.Error("the connector did not stop")
	}
	w.cancel = nil
}

func (w *removalWorker) enrolled() bool {
	_, ok, err := w.enr.Load(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	return ok
}

// keepSomething writes what a pairing wipe must not touch: a share selection
// and a session counter value. keptAfter asserts them, and worker.key.
func (w *removalWorker) keepSomething() (counter int64) {
	w.t.Helper()
	sel := wstore.ShareSelection{Mode: wstore.ShareSelect, Sessions: []string{"minion-1"}}
	if err := w.st.SaveShare(sel); err != nil {
		w.t.Fatal(err)
	}
	n, err := w.st.NextSessionNumber()
	if err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *removalWorker) keptAfter(counter int64) {
	w.t.Helper()
	if _, err := os.Stat(w.enr.KeyPath()); err != nil {
		w.t.Errorf("worker.key is gone after a revoke: %v", err)
	}
	got, err := w.st.LoadShare()
	if err != nil || got.Mode != wstore.ShareSelect || len(got.Sessions) != 1 || got.Sessions[0] != "minion-1" {
		w.t.Errorf("share selection after the revoke = %+v, %v", got, err)
	}
	n, err := w.st.NextSessionNumber()
	if err != nil || n != counter+1 {
		w.t.Errorf("session counter after the revoke = %d, %v; want %d", n, err, counter+1)
	}
}

func newRemover(t *testing.T, h *connHub, grace time.Duration) *workers.Remover {
	t.Helper()
	svc := h.service()
	rm, err := workers.NewRemover(workers.RemoveConfig{
		Store:   h.st,
		Auditor: audit.New(h.st),
		Live:    &workers.RegistryRevoker{Registry: svc.Registry(), Identity: svc.Identity(), Grace: grace},
	})
	if err != nil {
		t.Fatal(err)
	}
	return rm
}

func warningCodes(res workers.RemoveResult) []string {
	var out []string
	for _, w := range res.Warnings() {
		out = append(out, w.Code)
	}
	return out
}

func TestRemovingAnOnlineWorkerWithARevokeHandlerWipesItAtOnce(t *testing.T) {
	h := newConnHub(t)
	w := newRemovalWorker(t, h)
	counter := w.keepSomething()
	row := h.enrol(w.key.Public(), "pi")
	w.start(h, true)
	eventually(t, "the Worker to be online", func() bool { return h.service().Registry().IsOnline(row.ID) })
	eventually(t, "the connector to be connected", func() bool { return w.conn.Status().State == wagent.ConnConnected })

	res, err := newRemover(t, h, 5*time.Second).Remove(context.Background(), workers.RemoveActor{ID: "owner-1"}, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Online || !res.Revoked || len(res.Warnings()) != 0 {
		t.Fatalf("result = %+v, warnings %v; want online and confirmed with no warning", res, warningCodes(res))
	}
	eventually(t, "the Worker to wipe its enrollment", func() bool { return !w.enrolled() })
	eventually(t, "the connector to idle", func() bool { return w.conn.Status().State == wagent.ConnIdle })
	w.keptAfter(counter)

	// No further dial: the Hub never sees the Worker again, so the revoke of
	// the tombstone is not needed and is not asked for.
	attempts := w.conn.Status().Attempts
	time.Sleep(300 * time.Millisecond)
	if got := w.conn.Status().Attempts; got != attempts {
		t.Errorf("the wiped Worker dialed again: attempts %d -> %d", attempts, got)
	}
	if h.service().Registry().IsOnline(row.ID) {
		t.Error("the removed Worker is online")
	}
	if _, err := h.st.TombstoneByPub(context.Background(), w.key.Public()); err != nil {
		t.Errorf("no tombstone: %v", err)
	}
}

func TestRemovingAnOnlineWorkerWithoutARevokeHandlerWipesItWhenItReconnects(t *testing.T) {
	h := newConnHub(t)
	w := newRemovalWorker(t, h)
	counter := w.keepSomething()
	row := h.enrol(w.key.Public(), "pi")
	w.start(h, false) // today's production Worker: no op server on the link
	eventually(t, "the Worker to be online", func() bool { return h.service().Registry().IsOnline(row.ID) })
	eventually(t, "the connector to be connected", func() bool { return w.conn.Status().State == wagent.ConnConnected })

	res, err := newRemover(t, h, 200*time.Millisecond).Remove(context.Background(), workers.RemoveActor{ID: "owner-1"}, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Online || res.Revoked {
		t.Fatalf("result = %+v; want online and unconfirmed", res)
	}
	if got := warningCodes(res); len(got) != 1 || got[0] != ops.WarnRevokeNotConfirmed {
		t.Errorf("warnings = %v, want %s", got, ops.WarnRevokeNotConfirmed)
	}
	// The Hub ended the connection; the reconnect is answered with the
	// signed revoke of the tombstone.
	eventually(t, "the Worker to wipe its enrollment on the reconnect", func() bool { return !w.enrolled() })
	eventually(t, "the connector to idle", func() bool { return w.conn.Status().State == wagent.ConnIdle })
	w.keptAfter(counter)
	if h.service().Registry().IsOnline(row.ID) {
		t.Error("the removed Worker is online")
	}
}

func TestRemovingAnOfflineWorkerWipesItWhenItNextConnects(t *testing.T) {
	h := newConnHub(t)
	w := newRemovalWorker(t, h)
	counter := w.keepSomething()
	row := h.enrol(w.key.Public(), "pi")
	w.start(h, true)
	eventually(t, "the Worker to be online", func() bool { return h.service().Registry().IsOnline(row.ID) })
	w.stop() // the service is stopped while the Hub removes the Worker
	eventually(t, "the Hub to see the Worker go", func() bool { return !h.service().Registry().IsOnline(row.ID) })

	res, err := newRemover(t, h, time.Second).Remove(context.Background(), workers.RemoveActor{ID: "owner-1"}, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Online || res.Revoked {
		t.Fatalf("result = %+v; want offline", res)
	}
	if got := warningCodes(res); len(got) != 1 || got[0] != ops.WarnRevokeOnNextConnect {
		t.Errorf("warnings = %v, want %s", got, ops.WarnRevokeOnNextConnect)
	}
	if !w.enrolled() {
		t.Fatal("an offline Worker was wiped without a connection")
	}

	w.start(h, true)
	eventually(t, "the Worker to wipe its enrollment on connect", func() bool { return !w.enrolled() })
	eventually(t, "the connector to idle", func() bool { return w.conn.Status().State == wagent.ConnIdle })
	w.keptAfter(counter)
	if _, err := h.st.TombstoneByPub(context.Background(), w.key.Public()); err != nil {
		t.Errorf("no tombstone: %v", err)
	}
}
