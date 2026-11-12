//go:build e2e && unix

package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/adminsock"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/hub/server"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/transport"
	wagent "github.com/patrick-1984/ai-minion/internal/worker/agent"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
)

// The Hub identity life cycle of 0.7.0, composed: the default Hub (every
// production module, the real age sealer, the local admin socket) and a REAL
// Worker connector over the real protocol 3.0 handshake.
//
//  1. A Hub migrated from 0.6 (B1: plain identity.key, recorded root) with a
//     Worker that pinned that root under 0.6.
//  2. set-passphrase over the admin socket seals the root and issues the first
//     certificate (B3); the fingerprint does not change.
//  3. The Worker connects without any action and records the serial.
//  4. reissue over the socket ends the session; the Worker reconnects under the
//     new serial.
//  5. rotate-master over the socket ends the session; the Worker follows the
//     signed chain to the new root.
//  6. A new invitation names the new root.
//  7. The Hub restarts on the same data directory and still serves the Worker.

const (
	lcAdminPW = "identity-e2e-admin-pw-1"
	lcPass1   = "first long Hub passphrase e2e"
	lcPass2   = "second long Hub passphrase e2e"
)

type lifecycleHub struct {
	t      *testing.T
	dir    string
	addr   string
	st     *store.Store
	wsv    *workers.Service
	mgr    *session.Manager
	cancel context.CancelFunc
	done   chan error
}

func (h *lifecycleHub) start() {
	h.t.Helper()
	reg := server.DefaultRegistry.Clone()
	if err := reg.Register("grab-lifecycle", 101, func(_ context.Context, env *server.Env) error {
		var err error
		if h.st, err = server.Require[*store.Store](env); err != nil {
			return err
		}
		if h.wsv, err = server.Require[*workers.Service](env); err != nil {
			return err
		}
		h.mgr, err = server.Require[*session.Manager](env)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	listen := h.addr
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	cfg := &server.Config{DataDir: h.dir, Listen: listen, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := server.New(cfg, reg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	select {
	case <-srv.Started():
	case err := <-done:
		cancel()
		h.t.Fatalf("the Hub did not start: %v", err)
	case <-time.After(30 * time.Second):
		cancel()
		h.t.Fatal("the Hub did not start")
	}
	h.addr, h.cancel, h.done = srv.Addr(), cancel, done
}

func (h *lifecycleHub) stop() {
	h.t.Helper()
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("Run returned %v after shutdown", err)
		}
	case <-time.After(30 * time.Second):
		h.t.Error("the Hub did not stop")
	}
	h.cancel = nil
}

// socket sends one request to the Hub's local admin socket, as
// `minion hub identity` does.
func (h *lifecycleHub) socket(method, path string, body any) (int, []byte) {
	h.t.Helper()
	sock := adminsock.Path(h.dir)
	c := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://hub"+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (h *lifecycleHub) status() ops.HubIdentityStatusResult {
	h.t.Helper()
	code, body := h.socket("GET", adminsock.PathStatus, nil)
	var st ops.HubIdentityStatusResult
	if code != 200 || json.Unmarshal(body, &st) != nil {
		h.t.Fatalf("status = %d %s", code, body)
	}
	return st
}

func (h *lifecycleHub) identityOp(path string, body any) ops.HubIdentityResult {
	h.t.Helper()
	code, out := h.socket("POST", path, body)
	var res ops.HubIdentityResult
	if code != 200 || json.Unmarshal(out, &res) != nil {
		h.t.Fatalf("%s = %d %s", path, code, out)
	}
	return res
}

// rest calls a catalog operation as the administrator's browser session.
func (h *lifecycleHub) rest(op string, body any) (int, []byte) {
	h.t.Helper()
	root, err := h.st.UserByIdentifier(context.Background(), "root")
	if err != nil {
		h.t.Fatal(err)
	}
	is, err := h.mgr.Create(context.Background(), root, time.Hour)
	if err != nil {
		h.t.Fatal(err)
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "https://"+h.addr+"/api/v1/ops/"+op, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", session.CookieName+"="+is.Token)
	req.Header.Set(session.CSRFHeader, is.CSRFToken)
	c := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test Hub
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// migratedHubDir builds the data directory of a 0.6 Hub after the 0.7
// migrations (state B1): the plain root in identity.key and its public key
// recorded in the database. It returns the root.
func migratedHubDir(t *testing.T, dir string) *identity.Key {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	k, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.SaveKey(filepath.Join(dir, hubid.PlainFile), k); err != nil {
		t.Fatal(err)
	}
	if err := st.PutHubIdentity(context.Background(), store.HubIdentityRow{Pub: k.Public(), Fingerprint: k.Fingerprint().String(), CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE hub_identity SET migrated_from_06 = 1`); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestHubIdentityLifecycleFromAMigratedHubWithARealWorker(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", lcAdminPW)
	ctx := context.Background()

	// A short data directory: the admin socket path must fit a Unix socket.
	dir, err := os.MkdirTemp("", "mnlc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	// 1. The migrated Hub (B1) and its 0.6 Worker, which pinned the plain root.
	root06 := migratedHubDir(t, dir)
	h := &lifecycleHub{t: t, dir: dir}
	h.start()
	t.Cleanup(h.stop)
	if st := h.status(); st.State != "B1" || st.Serving || st.RootFingerprint != root06.Fingerprint().String() {
		t.Fatalf("B1 status = %+v", st)
	}
	owner, err := h.st.UserByIdentifier(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}

	wdir := t.TempDir()
	wst, err := wstore.Open(wdir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wst.Close() })
	enr := wagent.NewEnrollments(wst, wdir)
	wkey, err := enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := enr.Commit(ctx, wagent.Enrollment{Pin: identity.Pin{Pub: root06.Public()}}); err != nil {
		t.Fatal(err)
	}
	row := store.WorkerRow{ID: store.NewID(), Pub: wkey.Public(), Name: "pi", OwnerID: owner.ID,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli(), FirstConnectedAt: time.Now().UnixMilli()}
	if err := h.st.InsertWorker(ctx, nil, row); err != nil {
		t.Fatal(err)
	}
	logs := &syncLog{}
	conn, err := wagent.NewConnector(wagent.ConnectorConfig{
		HubURL:     "https://" + h.addr,
		Name:       "pi",
		Ops:        func() []string { return []string{"sessions_list"} },
		Key:        enr.LoadKey,
		Enrollment: enr,
		Policy:     wagent.NewPolicy(wst),
		Dialer:     wagent.NewHubDialer(),
		Backoff:    &transport.Backoff{Initial: 5 * time.Millisecond, Max: 100 * time.Millisecond},
		IdlePoll:   20 * time.Millisecond,
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	wctx, wcancel := context.WithCancel(ctx)
	wdone := make(chan struct{})
	go func() { defer close(wdone); conn.Run(wctx) }()
	t.Cleanup(func() {
		wcancel()
		select {
		case <-wdone:
		case <-time.After(10 * time.Second):
			t.Error("the connector did not stop")
		}
	})
	// A B1 Hub serves no Worker.
	time.Sleep(300 * time.Millisecond)
	if h.wsv.Registry().IsOnline(row.ID) {
		t.Fatal("a B1 Hub admitted a Worker")
	}

	online := func() (workers.Online, bool) { return h.wsv.Registry().Get(row.ID) }
	waitOnline := func(what string, since time.Time) workers.Online {
		t.Helper()
		var on workers.Online
		lifecycleEventually(t, what, func() bool {
			o, ok := online()
			on = o
			return ok && o.ConnectedAt.After(since)
		})
		return on
	}
	ratchet := func() (identity.Pin, transport.Ratchet) {
		e, ok, err := enr.Load(ctx)
		if err != nil || !ok {
			t.Fatalf("worker enrollment = %v, %v", ok, err)
		}
		return e.Pin, e.Ratchet
	}

	// 2. set-passphrase over the admin socket: sealed root, first certificate.
	first := h.identityOp(adminsock.PathSetPassphrase, map[string]any{"passphrase": lcPass1, "confirm": lcPass1})
	if first.RootFingerprint != root06.Fingerprint().String() || first.Serial == 0 {
		t.Fatalf("set-passphrase = %+v", first)
	}
	if _, err := os.Stat(filepath.Join(dir, hubid.PlainFile)); !os.IsNotExist(err) {
		t.Fatalf("the plain root is still on disk: %v", err)
	}
	if st := h.status(); st.State != "B3" || !st.Serving {
		t.Fatalf("status after set-passphrase = %+v", st)
	}

	// 3. The Worker connects by itself and records the serial.
	on1 := waitOnline("the Worker to connect after set-passphrase", time.Time{})
	lifecycleEventually(t, "the Worker to record the first serial", func() bool {
		_, r := ratchet()
		return r.MaxSerial == first.Serial
	})

	// 4. reissue: the session ends, the Worker comes back under the new serial.
	second := h.identityOp(adminsock.PathReissue, map[string]any{"passphrase": lcPass1, "validity_days": 90})
	if second.Serial <= first.Serial || second.RootFingerprint != first.RootFingerprint || second.OpFingerprint == first.OpFingerprint {
		t.Fatalf("reissue = %+v after %+v", second, first)
	}
	on2 := waitOnline("the Worker to reconnect after reissue", on1.ConnectedAt)
	lifecycleEventually(t, "the Worker to record the re-issued serial", func() bool {
		_, r := ratchet()
		return r.MaxSerial == second.Serial
	})

	// 5. rotate-master: the Worker follows the signed chain to the new root.
	third := h.identityOp(adminsock.PathRotateMaster, map[string]any{
		"passphrase": lcPass1, "new_passphrase": lcPass2, "confirm": lcPass2, "word": "ROTATE",
	})
	if third.RootFingerprint == first.RootFingerprint {
		t.Fatalf("rotate-master kept the root: %+v", third)
	}
	newRoot, err := identity.ParseFingerprint(third.RootFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	on3 := waitOnline("the Worker to reconnect after the rotation", on2.ConnectedAt)
	lifecycleEventually(t, "the Worker to follow the chain", func() bool {
		p, r := ratchet()
		return p.Seq == 1 && newRoot.Matches(p.Pub) && r.MaxSerial == third.Serial
	})

	// 6. A new invitation carries the new root's fingerprint.
	code, body := h.rest(ops.OpInvitationCreate, ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL1h, Name: "after-rotation"})
	var inv ops.InvitationCreateResult
	if code != 200 || json.Unmarshal(body, &inv) != nil {
		t.Fatalf("invitation_create = %d %s", code, body)
	}
	pk, err := identity.ParseInviteKey(inv.Key)
	if err != nil || pk.Fingerprint != newRoot {
		t.Fatalf("the invitation names %v, want the new root %v (%v)", pk.Fingerprint, newRoot, err)
	}

	// 7. A Hub restart on the same data directory still serves the Worker,
	// without a passphrase.
	h.stop()
	h.start()
	if st := h.status(); st.State != "B3" || !st.Serving || st.RootFingerprint != third.RootFingerprint || st.Serial != third.Serial {
		t.Fatalf("status after restart = %+v", st)
	}
	waitOnline("the Worker to reconnect after the Hub restart", on3.ConnectedAt)
	if p, r := ratchet(); !newRoot.Matches(p.Pub) || r.MaxSerial != third.Serial {
		t.Errorf("worker pin/ratchet after restart = %+v / %d", p, r.MaxSerial)
	}
}

func lifecycleEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
