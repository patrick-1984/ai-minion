package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptrace"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

// W9 Hub part with addendum C5: on the Hub's own TLS listener the welcome
// states the key of the certificate served on THAT connection: after a reload
// a connection that started earlier still reports its certificate, a new
// connection the new one. Slots do not outlive their connections.
func TestServedCertificateKeyIsPerConnection(t *testing.T) {
	dir := t.TempDir()
	cs, err := LoadCertSource(CertOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	hash1 := cs.KeyHash()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(workers.HubTLSOf(r))
	})}
	sc := newServedCerts()
	cfg := cs.TLSConfig()
	sc.install(srv, cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(tls.NewListener(ln, cfg)) }()
	t.Cleanup(func() { _ = srv.Close() })
	url := "https://" + ln.Addr().String() + "/"

	newClient := func() *http.Client {
		return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // test against a self-signed certificate
	}
	get := func(c *http.Client) (workers.HubTLS, [32]byte, bool) {
		t.Helper()
		reused := false
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused },
		}))
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out workers.HubTLS
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out, sha256.Sum256(resp.TLS.PeerCertificates[0].RawSubjectPublicKeyInfo), reused
	}

	a := newClient()
	got, seen, _ := get(a)
	if got.Mode != workers.HubTLSOwn || !bytes.Equal(got.KeyHash, hash1[:]) || seen != hash1 {
		t.Fatalf("first connection: %+v, seen %x, want %x", got, seen, hash1)
	}

	// A new certificate in the same files, reloaded (SIGHUP).
	tlsDir := filepath.Join(dir, TLSDir)
	if err := generateSelfSigned(tlsDir, filepath.Join(tlsDir, TLSCertFile), filepath.Join(tlsDir, TLSKeyFile), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := cs.Reload(); err != nil {
		t.Fatal(err)
	}
	hash2 := cs.KeyHash()
	if hash2 == hash1 {
		t.Fatal("the reload did not change the key")
	}
	got, seen, reused := get(a)
	if !reused || !bytes.Equal(got.KeyHash, hash1[:]) || seen != hash1 {
		t.Errorf("the earlier connection (reused %v) states %x, want %x", reused, got.KeyHash, hash1)
	}
	got, seen, _ = get(newClient())
	if !bytes.Equal(got.KeyHash, hash2[:]) || seen != hash2 {
		t.Errorf("a new connection states %x, want %x", got.KeyHash, hash2)
	}

	// A connection that ends before its handshake leaves no slot behind.
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sc.mu.Lock()
		n := len(sc.slots)
		sc.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d slots left", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTLSKeyPinFormat(t *testing.T) {
	var h [32]byte
	h[0] = 0xff
	if got := TLSKeyPin(h); got != "sha256///wAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" {
		t.Errorf("pin = %q", got)
	}
}

// Finding 8 of the round-1 review: a resumed TLS session skips
// GetCertificate, so the listener that reports the served key offers no
// resumption: a client with a session cache still gets a full handshake and
// the welcome states the key on every connection.
func TestServedCertificateKeyWithoutSessionResumption(t *testing.T) {
	cs, err := LoadCertSource(CertOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	want := cs.KeyHash()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(workers.HubTLSOf(r))
	})}
	cfg := cs.TLSConfig()
	newServedCerts().install(srv, cfg)
	if !cfg.SessionTicketsDisabled {
		t.Fatal("session tickets are still enabled on the reporting listener")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(tls.NewListener(ln, cfg)) }()
	t.Cleanup(func() { _ = srv.Close() })
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test against a self-signed certificate
		ClientSessionCache: tls.NewLRUClientSessionCache(4),
	}}}
	for i := 0; i < 3; i++ {
		resp, err := c.Get("https://" + ln.Addr().String() + "/")
		if err != nil {
			t.Fatal(err)
		}
		var got workers.HubTLS
		_ = json.NewDecoder(resp.Body).Decode(&got)
		_ = resp.Body.Close()
		if resp.TLS.DidResume || !bytes.Equal(got.KeyHash, want[:]) {
			t.Fatalf("connection %d: resumed %v, stated %x", i, resp.TLS.DidResume, got.KeyHash)
		}
	}
}
