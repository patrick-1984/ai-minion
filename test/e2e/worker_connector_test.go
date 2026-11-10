//go:build e2e

package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
	"github.com/patrick-1984/ai-minion/internal/transport"
	wagent "github.com/patrick-1984/ai-minion/internal/worker/agent"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Story 4.4 integration tests: the real Hub connect endpoint (Story 4.3, over an
// httptest TLS listener with a self-signed certificate for another host name)
// and the real Worker connector with the production dialer. The Worker has no
// TLS option, so every connection below proves that the certificate is never
// consulted (A-15). Run with "make e2e".

type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// connHub is the real Hub endpoint on a fixed address, restartable on the same
// data directory.
type connHub struct {
	t    *testing.T
	dir  string
	st   *store.Store
	cert tls.Certificate
	addr string
	// current is the Hub's protocol version; the zero value is proto.Current.
	// onStart runs on every (re)start with the new service, for the modes a
	// test enables (pairing). Both are set before a restart, never while the
	// Hub serves.
	current proto.Version
	onStart func(*workers.Service)

	mu  sync.Mutex
	svc *workers.Service
	srv *httptest.Server
}

func wrongNameCert(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "hub.other-host.invalid"},
		DNSNames:     []string{"hub.other-host.invalid"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

func newConnHub(t *testing.T) *connHub { return newConnHubWith(t, proto.Version{}, nil) }

// newConnHubWith starts the Hub at protocol version current (zero: the
// build's) and runs onStart on each service it starts.
func newConnHubWith(t *testing.T, current proto.Version, onStart func(*workers.Service)) *connHub {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &connHub{t: t, dir: dir, st: st, cert: wrongNameCert(t), current: current, onStart: onStart}
	h.start("127.0.0.1:0")
	t.Cleanup(h.stop)
	return h
}

func (h *connHub) start(addr string) {
	h.t.Helper()
	svc, err := workers.NewService(context.Background(), workers.ServiceOptions{
		DataDir: h.dir, Store: h.st, Limiter: openLimiter{},
		Logger: slog.New(slog.NewTextHandler(&syncLog{}, nil)), Current: h.current,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if h.onStart != nil {
		h.onStart(svc)
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		h.t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+transport.ConnectPath, svc.Handler())
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener.Close()
	srv.Listener = l
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{h.cert}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	h.mu.Lock()
	h.svc, h.srv, h.addr = svc, srv, l.Addr().String()
	h.mu.Unlock()
}

func (h *connHub) stop() {
	h.mu.Lock()
	svc, srv := h.svc, h.srv
	h.svc, h.srv = nil, nil
	h.mu.Unlock()
	if svc != nil {
		svc.Close()
	}
	if srv != nil {
		srv.CloseClientConnections()
		srv.Close()
	}
}

func (h *connHub) url() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return "https://" + h.addr
}

func (h *connHub) service() *workers.Service {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.svc
}

func (h *connHub) pin() identity.Pin { return identity.Pin{Pub: h.service().Identity().Public()} }

// enrol inserts the Worker row that enrollment (Story 4.5) will create.
func (h *connHub) enrol(key []byte, name string) store.WorkerRow {
	h.t.Helper()
	w := store.WorkerRow{
		ID: store.NewID(), Pub: key, Name: name, OwnerID: "owner-1",
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli(),
	}
	if err := h.st.InsertWorker(context.Background(), nil, w); err != nil {
		h.t.Fatal(err)
	}
	return w
}

// openLimiter admits every connect: the Worker retries fast in these tests.
type openLimiter struct{}

func (openLimiter) Allow(ratelimit.Policy, netip.Addr, string) (bool, time.Duration) {
	return true, 0
}

type e2eWorker struct {
	conn *wagent.Connector
	enr  *wagent.Enrollments
	key  *identity.Key
	logs *syncLog
}

func startWorker(t *testing.T, h *connHub) *e2eWorker {
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
	logs := &syncLog{}
	conn, err := wagent.NewConnector(wagent.ConnectorConfig{
		HubURL:     h.url(),
		Name:       "pi",
		Ops:        func() []string { return []string{"sessions_list", "send_text"} },
		Key:        enr.LoadKey,
		Enrollment: enr,
		Policy:     wagent.NewPolicy(st),
		Dialer:     wagent.NewHubDialer(),
		Backoff:    &transport.Backoff{Initial: 5 * time.Millisecond, Max: 40 * time.Millisecond},
		IdlePoll:   20 * time.Millisecond,
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); conn.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the connector did not stop")
		}
	})
	return &e2eWorker{conn: conn, enr: enr, key: key, logs: logs}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWorkerConnectsToTheRealHubWithASelfSignedWrongNameCertificateAndNoFlags(t *testing.T) {
	h := newConnHub(t)
	w := startWorker(t, h)
	row := h.enrol(w.key.Public(), "pi")

	eventually(t, "the Worker to be online at the Hub", func() bool { return h.service().Registry().IsOnline(row.ID) })
	on, _ := h.service().Registry().Get(row.ID)
	if !on.Announces("send_text") || on.Name != "pi" {
		t.Fatalf("online = %+v", on)
	}
	eventually(t, "the connector to be connected", func() bool { return w.conn.Status().State == wagent.ConnConnected })
	got, ok, err := w.enr.Load(context.Background())
	if err != nil || !ok || got.WorkerID != row.ID || got.Owner != "owner-1" || got.Kind != "private" {
		t.Fatalf("stored enrollment = %+v, %v, %v", got, ok, err)
	}
	if !strings.Contains(w.logs.String(), "connected to hub") {
		t.Fatalf("logs = %s", w.logs.String())
	}
}

func TestWorkerReconnectsWhenTheRealHubRestarts(t *testing.T) {
	h := newConnHub(t)
	w := startWorker(t, h)
	row := h.enrol(w.key.Public(), "pi")
	eventually(t, "the first connection", func() bool { return h.service().Registry().IsOnline(row.ID) })

	h.stop()
	eventually(t, "the Worker to notice the loss", func() bool { return w.conn.Status().State != wagent.ConnConnected })
	eventually(t, "several failed attempts while the Hub is down", func() bool { return w.conn.Status().Attempts >= 3 })
	h.mu.Lock()
	addr := h.addr
	h.mu.Unlock()
	h.start(addr)
	eventually(t, "the Worker to be online again", func() bool { return h.service().Registry().IsOnline(row.ID) })
	if _, ok, _ := w.enr.Load(context.Background()); !ok {
		t.Fatal("the enrollment must survive the restart")
	}
}

func TestUnknownWorkerKeepsItsStateAndTheHubRevokesARemovedOne(t *testing.T) {
	h := newConnHub(t)
	w := startWorker(t, h)
	eventually(t, "not_enrolled to be reported", func() bool {
		return strings.Contains(w.conn.Status().LastError, "not_enrolled")
	})
	if _, ok, _ := w.enr.Load(context.Background()); !ok {
		t.Fatal("not_enrolled must keep the enrollment")
	}

	row := h.enrol(w.key.Public(), "pi")
	eventually(t, "the Worker to connect once the Hub knows it", func() bool { return h.service().Registry().IsOnline(row.ID) })

	if err := h.st.TombstoneWorker(context.Background(), row.ID, "owner-1", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	h.service().Registry().Disconnect(row.ID)
	eventually(t, "the signed revoke to wipe the enrollment", func() bool {
		_, ok, _ := w.enr.Load(context.Background())
		return !ok
	})
	if !strings.Contains(w.logs.String(), "removed by the hub") {
		t.Fatalf("logs = %s", w.logs.String())
	}
}
