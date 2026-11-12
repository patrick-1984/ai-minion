package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubcert"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// stubHub is an in-process Hub made only of the shared transport package, as
// the Hub's connect endpoint (Story 4.3) uses it: WebSocket accept, AD-6
// handshake, one control frame inside the channel, then yamux. It serves a
// TLS listener whose self-signed certificate is issued for another host name
// than the one the Worker dials, so a Worker that consulted the certificate
// could not connect. The Worker side under test is the production dialer with
// no TLS option at all (A-15). The same flow against the real Hub is tested
// in test/e2e (build tag e2e).
type stubHub struct {
	t *testing.T
	// key is the ROOT key (what a Worker pins); hub holds the operational key and
	// the certificate the stub signs and presents with (protocol 3.0).
	key  *identity.Key
	hub  *hubcert.Hub
	addr string
	cert tls.Certificate

	mu         sync.Mutex
	srv        *httptest.Server
	known      map[string]bool // Worker public keys (string form) that are enrolled
	tombstones map[string]bool
	sessions   []*transport.Session
	hellos     []transport.WorkerHello
	// revokeKey, when set, signs revokes instead of the operational key.
	revokeKey *identity.Key

	connects atomic.Int32
	// onSession runs for every enrolled Worker session, in the handler.
	onSession func(*transport.Session)
}

func newStubHub(t *testing.T) *stubHub {
	t.Helper()
	hub := hubcert.New(t)
	h := &stubHub{t: t, key: hub.Root, hub: hub, known: map[string]bool{}, tombstones: map[string]bool{}, cert: wrongNameCert(t)}
	h.start("127.0.0.1:0")
	t.Cleanup(h.stop)
	return h
}

// wrongNameCert returns a self-signed certificate for a host name that is
// neither the address the Worker dials nor anything it could trust.
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

func (h *stubHub) start(addr string) {
	h.t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		h.t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+transport.ConnectPath, h.serve)
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener.Close()
	srv.Listener = l
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{h.cert}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	h.mu.Lock()
	h.srv, h.addr = srv, l.Addr().String()
	h.mu.Unlock()
}

func (h *stubHub) url() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return "https://" + h.addr
}

// stop ends every Worker session and closes the listener (a Hub restart).
func (h *stubHub) stop() {
	h.mu.Lock()
	srv, sessions := h.srv, h.sessions
	h.srv, h.sessions = nil, nil
	h.mu.Unlock()
	for _, s := range sessions {
		_ = s.Close()
	}
	if srv != nil {
		srv.CloseClientConnections()
		srv.Close()
	}
}

// config is the Hub configuration of one connection: the operational key and
// certificate in force now.
func (h *stubHub) config() transport.HubConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hub.Config()
}

// reissue makes a new operational key and certificate with the given serial.
func (h *stubHub) reissue(serial uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hub.Reissue(h.t, serial)
}

// dropSessions ends every Worker session (the Workers reconnect).
func (h *stubHub) dropSessions() {
	h.mu.Lock()
	sessions := h.sessions
	h.sessions = nil
	h.mu.Unlock()
	for _, s := range sessions {
		_ = s.Close()
	}
}

func (h *stubHub) helloCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.hellos)
}

func (h *stubHub) enrol(pub []byte)     { h.set(h.known, pub, true) }
func (h *stubHub) tombstone(pub []byte) { h.set(h.tombstones, pub, true) }

func (h *stubHub) set(m map[string]bool, pub []byte, v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m[string(pub)] = v
}

func (h *stubHub) lookup(pub []byte) (known, tombstoned bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.known[string(pub)], h.tombstones[string(pub)]
}

func (h *stubHub) lastHello() (transport.WorkerHello, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.hellos) == 0 {
		return transport.WorkerHello{}, false
	}
	return h.hellos[len(h.hellos)-1], true
}

func (h *stubHub) serve(w http.ResponseWriter, r *http.Request) {
	h.connects.Add(1)
	conn, err := transport.Accept(w, r)
	if err != nil {
		return
	}
	hc := h.config()
	hc.Timeout = 10 * time.Second
	res, err := transport.HubHandshake(conn, hc)
	if err != nil {
		return // the Worker did not accept this Hub (or the reverse)
	}
	h.mu.Lock()
	h.hellos = append(h.hellos, res.Worker)
	h.mu.Unlock()
	write := func(v any) error { return proto.WriteFrame(res.Channel, proto.StreamFrameLimit, v) }
	known, tomb := h.lookup(res.WorkerPub)
	switch {
	case tomb:
		signer := hc.Key
		h.mu.Lock()
		if h.revokeKey != nil {
			signer = h.revokeKey
		}
		h.mu.Unlock()
		sig, _ := signer.Sign(identity.PurposeRevoke, res.TranscriptHash[:])
		_ = write(map[string]any{"type": proto.TypeRevoke, "sig": identity.B64(sig)})
		_ = res.Channel.Close()
	case !known:
		_ = write(map[string]string{"type": proto.TypeNotEnrolled})
		_ = res.Channel.Close()
	default:
		if err := write(map[string]any{
			"type": proto.TypeWelcome, "worker_id": "w-" + res.Worker.Name, "name": res.Worker.Name, "owner": "alice",
			"kind": "private", "policy": map[string]bool{"preview_only": false},
			"proto_min": "1.0", "proto_max": "1.0",
		}); err != nil {
			return
		}
		sess, err := transport.NewSession(res.Channel, transport.RoleHub, transport.SessionOptions{})
		if err != nil {
			return
		}
		h.mu.Lock()
		h.sessions = append(h.sessions, sess)
		cb := h.onSession
		h.mu.Unlock()
		if cb != nil {
			go cb(sess)
		}
		<-sess.Done()
	}
}

// hubWorker is a Worker connector over a real store, with the production
// dialer and a fast real-time backoff.
type hubWorker struct {
	conn *Connector
	enr  *Enrollments
	st   *store.Store
	key  *identity.Key
	logs *logBuf
}

func newHubWorker(t *testing.T, hubURL string, pin identity.Pin) *hubWorker {
	t.Helper()
	return newHubWorkerDialer(t, hubURL, pin, NewHubDialer())
}

func newHubWorkerDialer(t *testing.T, hubURL string, pin identity.Pin, dialer Dialer) *hubWorker {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enr := NewEnrollments(st, dir)
	key, err := enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := enr.Commit(context.Background(), Enrollment{Pin: pin}); err != nil {
		t.Fatal(err)
	}
	logs := &logBuf{}
	conn, err := NewConnector(ConnectorConfig{
		HubURL:     hubURL,
		Name:       "pi",
		Ops:        func() []string { return []string{"sessions_list"} },
		Key:        enr.LoadKey,
		Enrollment: enr,
		Policy:     NewPolicy(st),
		Dialer:     dialer, // production: no TLS option exists, the certificate is never consulted
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
	return &hubWorker{conn: conn, enr: enr, st: st, key: key, logs: logs}
}

func (w *hubWorker) waitState(t *testing.T, want ConnState) {
	t.Helper()
	eventually(t, func() bool { return w.conn.Status().State == want })
}

func TestStubHubCertificateIsReallyForAnotherHostName(t *testing.T) {
	h := newStubHub(t)
	// A normal verifying client refuses this certificate: it is self-signed
	// and issued for another name. The Worker still connects (next tests).
	_, err := (&http.Client{Timeout: 5 * time.Second}).Get(h.url())
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostErr x509.HostnameError
	if err == nil || (!errors.As(err, &unknown) && !errors.As(err, &invalid) && !errors.As(err, &hostErr) &&
		!strings.Contains(err.Error(), "certificate")) {
		t.Fatalf("a verifying client must refuse the certificate, got %v", err)
	}
}

func TestSelfSignedWrongNameHubConnectsWithoutAnyTLSFlag(t *testing.T) {
	h := newStubHub(t)
	w := newHubWorker(t, h.url(), identity.Pin{Pub: h.key.Public()})
	h.enrol(w.key.Public())
	w.waitState(t, ConnConnected)

	if hello, ok := h.lastHello(); !ok || hello.Mode != transport.ModeConnect || hello.Name != "pi" ||
		len(hello.Ops) != 1 || hello.Ops[0] != "sessions_list" {
		t.Fatalf("hello = %+v", hello)
	}
	got, ok, err := w.enr.Load(context.Background())
	if err != nil || !ok || got.WorkerID != "w-pi" || got.Owner != "alice" {
		t.Fatalf("stored enrollment = %+v, %v, %v", got, ok, err)
	}
	if !strings.Contains(w.logs.String(), "connected to hub") {
		t.Fatalf("logs = %s", w.logs.String())
	}
}

func TestHubWithAnotherKeyIsNeverTrusted(t *testing.T) {
	h := newStubHub(t)
	impostor, _ := identity.GenerateKey()
	w := newHubWorker(t, h.url(), identity.Pin{Pub: impostor.Public()}) // pinned to a different Hub key
	h.enrol(w.key.Public())
	eventually(t, func() bool { return h.connects.Load() >= 3 })
	if w.conn.Status().State == ConnConnected {
		t.Fatal("a Hub that does not match the pin must never be connected to")
	}
	if _, ok, _ := w.enr.Load(context.Background()); !ok {
		t.Fatal("a failed authentication must not delete the enrollment")
	}
}

func TestReconnectsWhenTheHubRestarts(t *testing.T) {
	h := newStubHub(t)
	w := newHubWorker(t, h.url(), identity.Pin{Pub: h.key.Public()})
	h.enrol(w.key.Public())
	w.waitState(t, ConnConnected)

	h.stop()
	w.waitState(t, ConnBackoff)
	// The Hub is down for a while: the Worker keeps retrying.
	eventually(t, func() bool { return w.conn.Status().Attempts >= 3 })
	h.mu.Lock()
	addr := h.addr
	h.mu.Unlock()
	h.start(addr)
	w.waitState(t, ConnConnected)
	if _, ok, _ := w.enr.Load(context.Background()); !ok {
		t.Fatal("the enrollment must survive the outage")
	}
}

func TestNotEnrolledAtTheHubKeepsStateUntilTheHubKnowsTheWorker(t *testing.T) {
	h := newStubHub(t)
	w := newHubWorker(t, h.url(), identity.Pin{Pub: h.key.Public()})
	eventually(t, func() bool { return h.connects.Load() >= 2 })
	if st := w.conn.Status(); st.State == ConnConnected || !strings.Contains(st.LastError, "not_enrolled") {
		t.Fatalf("status = %+v", st)
	}
	if _, ok, _ := w.enr.Load(context.Background()); !ok {
		t.Fatal("not_enrolled must not delete the enrollment")
	}
	h.enrol(w.key.Public()) // a restored backup now knows the Worker again
	w.waitState(t, ConnConnected)
}

func TestSignedRevokeFromTheHubWipesTheEnrollment(t *testing.T) {
	h := newStubHub(t)
	w := newHubWorker(t, h.url(), identity.Pin{Pub: h.key.Public()})
	h.tombstone(w.key.Public())
	eventually(t, func() bool { _, ok, _ := w.enr.Load(context.Background()); return !ok })
	eventually(t, func() bool { st := w.conn.Status(); return st.State == ConnIdle && st.Reason == "not enrolled" })
	if !strings.Contains(w.logs.String(), "removed by the hub") {
		t.Fatalf("logs = %s", w.logs.String())
	}
	if _, err := w.enr.LoadKey(); err != nil {
		t.Fatalf("the key stays: %v", err)
	}
	n := h.connects.Load()
	time.Sleep(100 * time.Millisecond)
	if h.connects.Load() != n {
		t.Fatal("a removed Worker must stop dialing")
	}
}

func TestHubOpensAnOpStreamOnTheWelcomedLink(t *testing.T) {
	h := newStubHub(t)
	w := newHubWorker(t, h.url(), identity.Pin{Pub: h.key.Public()})
	got := make(chan json.RawMessage, 1)
	w.conn.OnLink("echo", func(l *Link) {
		st, err := l.Session.Accept(l.Context())
		if err != nil {
			return
		}
		body, err := st.ReadFrame()
		if err == nil {
			got <- body
		}
		st.Reset()
	})
	h.mu.Lock()
	h.onSession = func(s *transport.Session) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		st, err := s.OpenOp(ctx)
		if err != nil {
			return
		}
		_ = st.WriteFrame(map[string]string{"type": proto.TypeOpRequest, "op": "sessions_list"})
	}
	h.mu.Unlock()
	h.enrol(w.key.Public())
	select {
	case body := <-got:
		if !strings.Contains(string(body), "op.request") {
			t.Fatalf("frame = %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the op stream of the Hub did not reach the link handler")
	}
}
