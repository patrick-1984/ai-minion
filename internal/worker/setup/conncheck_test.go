package setup

import (
	"context"
	"crypto/sha1" //nolint:gosec // see upgradeWithoutSubprotocol
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/transport"
)

// hubServer is an httptest server that behaves like a Hub for the protocol
// check: it accepts the Worker WebSocket and records what the check sent.
type hubServer struct {
	*httptest.Server
	mu       sync.Mutex
	accepted int
	received int // bytes the client sent after the upgrade
}

func (h *hubServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+transport.ConnectPath, func(w http.ResponseWriter, r *http.Request) {
		conn, err := transport.Accept(w, r)
		if err != nil {
			return
		}
		h.mu.Lock()
		h.accepted++
		h.mu.Unlock()
		buf := make([]byte, 64)
		n, _ := conn.Read(buf) // returns when the check closes the connection
		h.mu.Lock()
		h.received += n
		h.mu.Unlock()
		_ = conn.Close()
	})
	return mux
}

func (h *hubServer) counts() (accepted, received int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.accepted, h.received
}

func newHub(t *testing.T) *hubServer {
	t.Helper()
	h := &hubServer{}
	h.Server = httptest.NewUnstartedServer(h.handler())
	// The verified handshake of the check fails on purpose against a
	// self-signed Hub; the server's own log of that is noise.
	h.Config.ErrorLog = log.New(io.Discard, "", 0)
	h.StartTLS()
	t.Cleanup(h.Close)
	return h
}

func poolOf(srv *httptest.Server) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(srv.Certificate())
	return p
}

func fpOf(srv *httptest.Server) string {
	sum := sha256.Sum256(srv.Certificate().Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return "SHA-256 " + strings.Join(parts, ":")
}

func stage(t *testing.T, r Report, name string) Stage {
	t.Helper()
	s, ok := r.Stage(name)
	if !ok {
		t.Fatalf("report has no %s stage: %+v", name, r.Stages)
	}
	return s
}

func wantStages(t *testing.T, r Report, want ...Status) {
	t.Helper()
	if len(r.Stages) != 4 {
		t.Fatalf("stages = %+v, want four", r.Stages)
	}
	for i, w := range want {
		if r.Stages[i].Status != w {
			t.Errorf("%s: status %s, want %s (%s)", r.Stages[i].Name, r.Stages[i].Status, w, r.Stages[i].Message)
		}
	}
}

func noProxy(*url.URL) (*url.URL, error) { return nil, nil }

// dialLoopback is the Worker's real dial, with the host of the address mapped
// to the loopback server (the stages before it use the injected DNS).
func dialLoopback(host string) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, hubURL string) (net.Conn, error) {
		return transport.Dial(ctx, strings.Replace(hubURL, host, "127.0.0.1", 1))
	}
}

// A Hub with a certificate this computer trusts: every stage passes and the
// certificate counts as trusted.
func TestCheckOK(t *testing.T) {
	hub := newHub(t)
	c := &Checker{RootCAs: poolOf(hub.Server), Proxy: noProxy, DialHub: dialLoopback("example.com"),
		LookupHost: func(_ context.Context, host string) ([]string, error) {
			if host != "example.com" {
				t.Errorf("looked up %q", host)
			}
			return []string{"127.0.0.1"}, nil
		}}
	_, port, _ := net.SplitHostPort(hub.Listener.Addr().String())
	rep := c.Check(context.Background(), "https://example.com:"+port)
	wantStages(t, rep, StatusOK, StatusOK, StatusOK, StatusOK)
	if !rep.CertTrusted {
		t.Error("a verified certificate must count as trusted")
	}
	if rep.CertFingerprint != fpOf(hub.Server) {
		t.Errorf("fingerprint = %q, want %q", rep.CertFingerprint, fpOf(hub.Server))
	}
	if s := stage(t, rep, StageDNS); !strings.Contains(s.Message, "example.com was found") {
		t.Errorf("DNS message: %q", s.Message)
	}
	if s := stage(t, rep, StageProtocol); !strings.Contains(s.Message, "minion.v1") {
		t.Errorf("protocol message: %q", s.Message)
	}
	accepted, received := hub.counts()
	if accepted != 1 {
		t.Errorf("the Hub accepted %d connections, want 1", accepted)
	}
	if received != 0 {
		t.Errorf("the check sent %d bytes after the upgrade; it must send nothing (no hello, no Hub state)", received)
	}
}

// A self-signed Hub is the normal case of the lab: the check must not pretend
// that the certificate verified, must show the fingerprint, and must still
// pass the protocol stage, because the Worker's own dial ignores the certificate (A-15).
func TestCheckSelfSignedIsAWarningWithFingerprintNeverTrusted(t *testing.T) {
	hub := newHub(t) // its certificate is not in the system trust store
	c := &Checker{Proxy: noProxy}
	rep := c.Check(context.Background(), hub.URL)
	wantStages(t, rep, StatusOK, StatusOK, StatusWarning, StatusOK)
	if rep.CertTrusted {
		t.Fatal("a self-signed certificate must never be reported as trusted")
	}
	if rep.CertFingerprint != fpOf(hub.Server) {
		t.Errorf("fingerprint = %q, want %q", rep.CertFingerprint, fpOf(hub.Server))
	}
	msg := stage(t, rep, StageTLS).Message
	for _, want := range []string{"not trusted", "self-signed", fpOf(hub.Server)} {
		if !strings.Contains(msg, want) {
			t.Errorf("TLS message lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "is trusted by this computer") {
		t.Errorf("TLS message claims trust: %s", msg)
	}
}

// hub_insecure switches the trust check off. The result is a warning, never "trusted".
func TestCheckInsecureSettingNeverClaimsTrust(t *testing.T) {
	hub := newHub(t)
	rep := (&Checker{Insecure: true, RootCAs: poolOf(hub.Server), Proxy: noProxy}).Check(context.Background(), hub.URL)
	wantStages(t, rep, StatusOK, StatusOK, StatusWarning, StatusOK)
	if rep.CertTrusted {
		t.Error("with certificate checking off the certificate must not count as trusted")
	}
	if msg := stage(t, rep, StageTLS).Message; !strings.Contains(msg, "switched off") || !strings.Contains(msg, fpOf(hub.Server)) {
		t.Errorf("TLS message: %s", msg)
	}
}

// A certificate for another name is not trusted either, and the message says why.
func TestCheckWrongNameCertificate(t *testing.T) {
	hub := newHub(t)
	_, port, _ := net.SplitHostPort(hub.Listener.Addr().String())
	c := &Checker{RootCAs: poolOf(hub.Server), Proxy: noProxy, DialHub: dialLoopback("hub.not-in-the-certificate.test"),
		LookupHost: func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }}
	rep := c.Check(context.Background(), "https://hub.not-in-the-certificate.test:"+port)
	wantStages(t, rep, StatusOK, StatusOK, StatusWarning, StatusOK)
	if rep.CertTrusted {
		t.Error("a certificate for another name must not count as trusted")
	}
	if msg := stage(t, rep, StageTLS).Message; !strings.Contains(msg, "different name") {
		t.Errorf("TLS message: %s", msg)
	}
}

// Wrong port: nothing listens. The stage says so in plain English and the
// later stages are skipped.
func TestCheckWrongPortConnectionRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // the port is free now: refused
	rep := (&Checker{Proxy: noProxy}).Check(context.Background(), "https://"+addr)
	wantStages(t, rep, StatusOK, StatusFailed, StatusSkipped, StatusSkipped)
	if msg := stage(t, rep, StageTCP).Message; !strings.Contains(msg, "nothing listens") || !strings.Contains(msg, "port") {
		t.Errorf("TCP message: %s", msg)
	}
	if !rep.Failed() {
		t.Error("Failed() = false")
	}
}

// A plain HTTP server behind an https:// address is the classic TLS error.
func TestCheckTLSErrorOnAPlainHTTPPort(t *testing.T) {
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	https := "https://" + strings.TrimPrefix(plain.URL, "http://")
	rep := (&Checker{Proxy: noProxy}).Check(context.Background(), https)
	wantStages(t, rep, StatusOK, StatusOK, StatusFailed, StatusSkipped)
	if msg := stage(t, rep, StageTLS).Message; !strings.Contains(msg, "does not speak HTTPS") {
		t.Errorf("TLS message: %s", msg)
	}
	if rep.CertFingerprint != "" || rep.CertTrusted {
		t.Errorf("no certificate was seen: %+v", rep)
	}
}

// A server that accepts the connection and then says nothing ends at the
// stage deadline, with a readable message, instead of hanging.
func TestCheckTLSStallEndsAtTheStageDeadline(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { <-stop; _ = c.Close() }()
		}
	}()
	t.Cleanup(func() { close(stop); _ = l.Close(); wg.Wait() })
	c := &Checker{StageTimeout: 150 * time.Millisecond, Proxy: noProxy}
	rep := c.Check(context.Background(), "https://"+l.Addr().String())
	wantStages(t, rep, StatusOK, StatusOK, StatusFailed, StatusSkipped)
	if msg := stage(t, rep, StageTLS).Message; !strings.Contains(msg, "did not finish") {
		t.Errorf("TLS message: %s", msg)
	}
}

// An HTTPS server that is no Hub: a plain page, a 404 and a WebSocket server
// with another protocol each get their own explanation.
func TestCheckWrongProtocol(t *testing.T) {
	cases := []struct {
		name    string
		handler http.Handler
		want    string
	}{
		{"plain page", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hello")) }), "HTTP 200"},
		{"not found", http.NotFoundHandler(), "HTTP 404"},
		{"other websocket protocol", http.HandlerFunc(upgradeWithoutSubprotocol), "does not speak the Minion protocol"},
		{"proxy without a Hub", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }), "HTTP 502"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(tc.handler)
			defer srv.Close()
			rep := (&Checker{Proxy: noProxy}).Check(context.Background(), srv.URL)
			wantStages(t, rep, StatusOK, StatusOK, StatusWarning, StatusFailed)
			if msg := stage(t, rep, StageProtocol).Message; !strings.Contains(msg, tc.want) {
				t.Errorf("protocol message %q lacks %q", msg, tc.want)
			}
		})
	}
}

// upgradeWithoutSubprotocol completes a WebSocket upgrade but selects no
// subprotocol: a WebSocket server that is not a Hub.
func upgradeWithoutSubprotocol(w http.ResponseWriter, r *http.Request) {
	const magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + magic)) //nolint:gosec // the WebSocket handshake of RFC 6455 requires SHA-1
	h, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, rw, err := h.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		base64.StdEncoding.EncodeToString(sum[:]))
	_ = rw.Flush()
}

// A path prefix (a reverse proxy) is kept: the check must reach the Hub there.
func TestCheckKeepsAPathPrefix(t *testing.T) {
	mux := http.NewServeMux()
	inner := &hubServer{}
	mux.Handle("/minion/", http.StripPrefix("/minion", inner.handler()))
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	rep := (&Checker{Proxy: noProxy}).Check(context.Background(), srv.URL+"/minion")
	wantStages(t, rep, StatusOK, StatusOK, StatusWarning, StatusOK)
}

// Plain http: no TLS stage can pass, and the message says the link is not
// encrypted instead of pretending.
func TestCheckPlainHTTPHub(t *testing.T) {
	h := &hubServer{}
	srv := httptest.NewServer(h.handler())
	defer srv.Close()
	rep := (&Checker{Proxy: noProxy}).Check(context.Background(), srv.URL)
	wantStages(t, rep, StatusOK, StatusOK, StatusWarning, StatusOK)
	if msg := stage(t, rep, StageTLS).Message; !strings.Contains(msg, "not encrypted") {
		t.Errorf("TLS message: %s", msg)
	}
	if rep.CertFingerprint != "" || rep.CertTrusted {
		t.Errorf("plain http has no certificate: %+v", rep)
	}
}

func TestCheckDNSFailure(t *testing.T) {
	c := &Checker{Proxy: noProxy, LookupHost: func(_ context.Context, host string) ([]string, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}}
	rep := c.Check(context.Background(), "https://hub.nowhere.invalid:8443")
	wantStages(t, rep, StatusFailed, StatusSkipped, StatusSkipped, StatusSkipped)
	if msg := stage(t, rep, StageDNS).Message; !strings.Contains(msg, "does not exist") || !strings.Contains(msg, "hub.nowhere.invalid") {
		t.Errorf("DNS message: %s", msg)
	}
}

func TestCheckDNSTimeoutUsesTheStageDeadline(t *testing.T) {
	c := &Checker{Proxy: noProxy, StageTimeout: 50 * time.Millisecond, LookupHost: func(ctx context.Context, _ string) ([]string, error) {
		<-ctx.Done() // a resolver that never answers; the stage deadline ends it
		return nil, ctx.Err()
	}}
	rep := c.Check(context.Background(), "https://slow.example:8443")
	wantStages(t, rep, StatusFailed, StatusSkipped, StatusSkipped, StatusSkipped)
	if msg := stage(t, rep, StageDNS).Message; !strings.Contains(msg, "took too long") {
		t.Errorf("DNS message: %s", msg)
	}
}

func TestCheckCancelledContext(t *testing.T) {
	hub := newHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep := (&Checker{Proxy: noProxy}).Check(ctx, hub.URL)
	if !rep.Failed() {
		t.Fatalf("a cancelled check must not pass: %+v", rep.Stages)
	}
	if accepted, _ := hub.counts(); accepted != 0 {
		t.Errorf("a cancelled check reached the Hub %d times", accepted)
	}
}

func TestCheckRejectsAnUnusableAddress(t *testing.T) {
	rep := (&Checker{Proxy: noProxy}).Check(context.Background(), "https://")
	if !rep.Failed() || len(rep.Stages) != 4 {
		t.Errorf("report: %+v", rep)
	}
}

// The report never repeats credentials of the address.
func TestCheckReportHidesCredentials(t *testing.T) {
	rep := (&Checker{Proxy: noProxy, LookupHost: func(context.Context, string) ([]string, error) {
		return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
	}}).Check(context.Background(), "https://user:s3cret@hub.example:8443")
	if strings.Contains(fmt.Sprint(rep), "s3cret") {
		t.Errorf("the report holds the password: %+v", rep)
	}
}

// A configured proxy is reported: DNS, TCP and TLS are direct checks.
func TestCheckNotesAProxy(t *testing.T) {
	hub := newHub(t)
	proxy := func(*url.URL) (*url.URL, error) { return url.Parse("http://proxy.example:3128") }
	rep := (&Checker{Proxy: proxy}).Check(context.Background(), hub.URL)
	if len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "proxy.example:3128") {
		t.Errorf("notes: %v", rep.Notes)
	}
}

// Real name resolution of the loopback name works without any injected fake.
func TestCheckResolvesLocalhostWithTheSystemResolver(t *testing.T) {
	hub := newHub(t)
	_, port, _ := net.SplitHostPort(hub.Listener.Addr().String())
	rep := (&Checker{Proxy: noProxy}).Check(context.Background(), "https://localhost:"+port)
	if s := stage(t, rep, StageDNS); s.Status != StatusOK {
		t.Skipf("localhost does not resolve here: %s", s.Message)
	}
	if s := stage(t, rep, StageTCP); s.Status == StatusFailed {
		t.Skipf("the server listens on 127.0.0.1 only and localhost resolved elsewhere: %s", s.Message)
	}
	if rep.CertFingerprint != fpOf(hub.Server) {
		t.Errorf("fingerprint = %q", rep.CertFingerprint)
	}
}
