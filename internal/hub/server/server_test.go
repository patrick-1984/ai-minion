package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
)

// ---- helpers ---------------------------------------------------------------

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func testLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func readCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		t.Fatalf("%s holds no PEM block", path)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func mustPrefixes(t *testing.T, list string) []netip.Prefix {
	t.Helper()
	p, err := ParseTrustedProxies(list)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func reqFrom(remote string, hdr ...string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://hub.test/", nil)
	r.RemoteAddr = remote
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Add(hdr[i], hdr[i+1])
	}
	return r
}

// ---- TLS: self-signed ------------------------------------------------------

func TestSelfSignedGeneratedOnceAndReused(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadCertSource(CertOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Generated || !first.SelfSigned {
		t.Fatalf("first start: Generated=%v SelfSigned=%v", first.Generated, first.SelfSigned)
	}
	certPath := filepath.Join(dir, TLSDir, TLSCertFile)
	keyPath := filepath.Join(dir, TLSDir, TLSKeyFile)

	leaf := readCert(t, certPath)
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Fatalf("public key = %T on %v, want ECDSA P-256", leaf.PublicKey, pub)
	}
	if leaf.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Errorf("signature algorithm = %v", leaf.SignatureAlgorithm)
	}
	for _, name := range []string{"localhost", "127.0.0.1", "::1"} {
		if err := leaf.VerifyHostname(name); err != nil {
			t.Errorf("certificate does not cover %s: %v", name, err)
		}
	}
	if fi, err := os.Stat(keyPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v (err %v), want 0600", fi.Mode().Perm(), err)
	}
	if fi, err := os.Stat(filepath.Join(dir, TLSDir)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("tls dir mode = %v (err %v), want 0700", fi.Mode().Perm(), err)
	}
	if got, want := first.Fingerprint(), CertFingerprint(leaf.Raw); got != want {
		t.Errorf("Fingerprint = %s, want %s", got, want)
	}
	if fp := first.Fingerprint(); len(fp) != 32*3-1 || fp != strings.ToUpper(fp) {
		t.Errorf("fingerprint %q is not colon-separated upper-case SHA-256", fp)
	}

	second, err := LoadCertSource(CertOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if second.Generated {
		t.Error("second start regenerated the certificate")
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Error("the certificate changed between starts")
	}
}

func TestTLSConfigRequiresTLS13(t *testing.T) {
	cs, err := LoadCertSource(CertOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := cs.TLSConfig().MinVersion; got != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %#x, want TLS 1.3", got)
	}
}

func TestSelfSignedCoversConfiguredNamesAndIsRenewed(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadCertSource(CertOptions{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	before := readCert(t, filepath.Join(dir, TLSDir, TLSCertFile))

	// A new public host name: the generated certificate is replaced.
	cs, err := LoadCertSource(CertOptions{DataDir: dir, Names: []string{"hub.example.com", "10.1.2.3"}})
	if err != nil {
		t.Fatal(err)
	}
	if !cs.Generated {
		t.Fatal("certificate not regenerated for a name it does not cover")
	}
	leaf := readCert(t, filepath.Join(dir, TLSDir, TLSCertFile))
	for _, n := range []string{"hub.example.com", "10.1.2.3", "localhost"} {
		if err := leaf.VerifyHostname(n); err != nil {
			t.Errorf("new certificate does not cover %s: %v", n, err)
		}
	}
	if string(before.Raw) == string(leaf.Raw) {
		t.Error("certificate unchanged")
	}

	// Same names again: reused.
	again, err := LoadCertSource(CertOptions{DataDir: dir, Names: []string{"hub.example.com"}})
	if err != nil || again.Generated {
		t.Fatalf("reuse with covered name: err=%v generated=%v", err, again != nil && again.Generated)
	}

	// Close to expiry: replaced.
	future := time.Now().Add(selfSignedValidity - renewBefore/2)
	renewed, err := LoadCertSource(CertOptions{DataDir: dir, Now: func() time.Time { return future }})
	if err != nil || !renewed.Generated {
		t.Fatalf("expiring certificate: err=%v generated=%v", err, renewed != nil && renewed.Generated)
	}
}

func TestBrokenGeneratedFilesAreNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	tlsDir := filepath.Join(dir, TLSDir)
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cert := filepath.Join(tlsDir, TLSCertFile)
	key := filepath.Join(tlsDir, TLSKeyFile)

	// Only one file.
	if err := os.WriteFile(key, []byte("lonely"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertSource(CertOptions{DataDir: dir}); err == nil || !strings.Contains(err.Error(), "remove both") {
		t.Fatalf("lone key: %v", err)
	}
	// Both, but garbage.
	if err := os.WriteFile(cert, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertSource(CertOptions{DataDir: dir}); err == nil || !strings.Contains(err.Error(), "cannot use the generated certificate") {
		t.Fatalf("garbage pair: %v", err)
	}
	if b, _ := os.ReadFile(key); string(b) != "lonely" {
		t.Error("the key file was overwritten")
	}
}

// ---- TLS: own certificate and reload --------------------------------------

func ownPair(t *testing.T, name string) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	if err := generateSelfSigned(dir, filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem"), []string{name}, time.Now()); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
}

func TestOwnCertificateIsUsedAndReloaded(t *testing.T) {
	cert, key := ownPair(t, "own.example")
	data := t.TempDir()
	cs, err := LoadCertSource(CertOptions{DataDir: data, CertFile: cert, KeyFile: key})
	if err != nil {
		t.Fatal(err)
	}
	if cs.SelfSigned || cs.Generated {
		t.Fatal("an operator certificate must not count as generated")
	}
	if _, err := os.Stat(filepath.Join(data, TLSDir)); !os.IsNotExist(err) {
		t.Error("the Hub created <data>/tls although it uses the operator's certificate")
	}
	if err := readCert(t, cert).VerifyHostname("own.example"); err != nil {
		t.Fatal(err)
	}
	fp1 := cs.Fingerprint()

	// Replace the files in place, then reload (what SIGHUP does).
	newCert, newKey := ownPair(t, "own.example")
	copyFile(t, newCert, cert)
	copyFile(t, newKey, key)
	if cs.Fingerprint() != fp1 {
		t.Fatal("the certificate changed before the reload")
	}
	if err := cs.Reload(); err != nil {
		t.Fatal(err)
	}
	if cs.Fingerprint() == fp1 {
		t.Fatal("reload did not pick up the new certificate")
	}

	// A broken replacement keeps the previous certificate.
	fp2 := cs.Fingerprint()
	if err := os.WriteFile(cert, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cs.Reload(); err == nil {
		t.Fatal("reload of a broken certificate succeeded")
	}
	if cs.Fingerprint() != fp2 {
		t.Fatal("a failed reload replaced the certificate")
	}
}

func TestCertAndKeyFileMustComeTogether(t *testing.T) {
	if _, err := LoadCertSource(CertOptions{DataDir: t.TempDir(), CertFile: "/x/cert.pem"}); err == nil {
		t.Fatal("cert file without key file accepted")
	}
	if _, err := LoadCertSource(CertOptions{DataDir: t.TempDir(), CertFile: "/nonexistent/c.pem", KeyFile: "/nonexistent/k.pem"}); err == nil {
		t.Fatal("missing own certificate accepted")
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---- configuration matrix --------------------------------------------------

func TestNewConfigMatrix(t *testing.T) {
	base := config.HubServer{DataDir: "/data", Listen: ":8443", TLS: config.TLSAuto}
	with := func(f func(*config.HubServer)) config.HubServer { h := base; f(&h); return h }

	cases := []struct {
		name    string
		hs      config.HubServer
		wantErr string // substring; empty means success
	}{
		{"defaults serve HTTPS", base, ""},
		{"off without public URL is refused", with(func(h *config.HubServer) { h.TLS = config.TLSOff }), "MINION_TLS=off needs MINION_PUBLIC_URL"},
		{"off with public URL serves HTTP", with(func(h *config.HubServer) { h.TLS = config.TLSOff; h.PublicURL = "https://hub.example.com" }), ""},
		{"off with own certificate conflicts", with(func(h *config.HubServer) {
			h.TLS, h.PublicURL, h.CertFile, h.KeyFile = config.TLSOff, "https://h.example", "c", "k"
		}), "conflicts"},
		{"own certificate", with(func(h *config.HubServer) { h.CertFile, h.KeyFile = "c", "k" }), ""},
		{"cert without key", with(func(h *config.HubServer) { h.CertFile = "c" }), "both"},
		{"bad public URL", with(func(h *config.HubServer) { h.PublicURL = "hub.example.com" }), "MINION_PUBLIC_URL"},
		{"bad proxies", with(func(h *config.HubServer) { h.TrustedProxies = "10.0.0.0/8, nonsense" }), "MINION_TRUSTED_PROXIES"},
		{"bad listen", with(func(h *config.HubServer) { h.Listen = "8443" }), "MINION_LISTEN"},
		{"bad port", with(func(h *config.HubServer) { h.Listen = ":99999" }), "invalid port"},
		{"empty listen falls back", with(func(h *config.HubServer) { h.Listen = "" }), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := NewConfig(tc.hs, nil)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Listen == "" {
					t.Error("Listen empty")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestResolveHubServerDefaultsAndEnv(t *testing.T) {
	res := &config.Resolver{Role: config.ScopeHub, LookupEnv: func(k string) (string, bool) {
		v, ok := map[string]string{
			"MINION_DATA_DIR":        "/data",
			"MINION_TLS_CERT_FILE":   "/c.pem",
			"MINION_TLS_KEY_FILE":    "/k.pem",
			"MINION_TRUSTED_PROXIES": "10.0.0.0/8",
		}[k]
		return v, ok
	}}
	hs, err := config.ResolveHubServer(res)
	if err != nil {
		t.Fatal(err)
	}
	if hs.Listen != ":8443" || hs.TLS != config.TLSAuto || hs.CertFile != "/c.pem" || hs.KeyFile != "/k.pem" || hs.TrustedProxies != "10.0.0.0/8" {
		t.Fatalf("resolved %+v", hs)
	}
}

// ---- public URL, client IP, trusted proxies --------------------------------

func TestNormalizePublicURL(t *testing.T) {
	ok := map[string]string{
		"":                           "",
		"https://hub.example.com":    "https://hub.example.com",
		"https://Hub.Example.com/":   "https://hub.example.com",
		"http://10.0.0.5:8080":       "http://10.0.0.5:8080",
		"  https://hub.example.com ": "https://hub.example.com",
	}
	for in, want := range ok {
		got, err := NormalizePublicURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizePublicURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"hub.example.com", "ftp://x.example", "https://", "https://u:p@hub.example.com", "https://hub.example.com/app", "https://hub.example.com?x=1", "https://hub.example.com#f", "//hub"} {
		if _, err := NormalizePublicURL(in); err == nil {
			t.Errorf("NormalizePublicURL(%q) accepted", in)
		} else if strings.Contains(in, "u:p") && strings.Contains(err.Error(), "u:p") {
			t.Errorf("error leaks credentials: %v", err)
		}
	}
}

func TestParseTrustedProxies(t *testing.T) {
	got, err := ParseTrustedProxies(" 10.0.0.0/8, 192.168.1.7 ,, fd00::/8, ::ffff:172.16.0.0/108")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0/8", "192.168.1.7/32", "fd00::/8", "172.16.0.0/12"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("entry %d = %s, want %s", i, got[i], want[i])
		}
	}
	if p, err := ParseTrustedProxies(""); err != nil || len(p) != 0 {
		t.Errorf("empty list: %v %v", p, err)
	}
	if _, err := ParseTrustedProxies("10.0.0.0/33"); err == nil {
		t.Error("bad prefix length accepted")
	}
}

func TestClientIP(t *testing.T) {
	n, err := NewNetwork("", true, mustPrefixes(t, "10.0.0.0/8, 192.168.5.5, fd00::/8"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		r    *http.Request
		want string
	}{
		{"direct client", reqFrom("203.0.113.9:1234"), "203.0.113.9"},
		{"direct client cannot spoof", reqFrom("203.0.113.9:1234", "X-Forwarded-For", "1.2.3.4"), "203.0.113.9"},
		{"untrusted proxy cannot spoof", reqFrom("198.51.100.1:80", "X-Forwarded-For", "1.2.3.4"), "198.51.100.1"},
		{"trusted proxy single hop", reqFrom("10.1.1.1:80", "X-Forwarded-For", "203.0.113.9"), "203.0.113.9"},
		{"client-supplied entries to the left are ignored", reqFrom("10.1.1.1:80", "X-Forwarded-For", "6.6.6.6, 203.0.113.9"), "203.0.113.9"},
		{"chain of trusted proxies", reqFrom("10.1.1.1:80", "X-Forwarded-For", "203.0.113.9, 10.2.2.2, 192.168.5.5"), "203.0.113.9"},
		{"several header lines", reqFrom("10.1.1.1:80", "X-Forwarded-For", "6.6.6.6", "X-Forwarded-For", "203.0.113.9"), "203.0.113.9"},
		{"no header from a trusted proxy", reqFrom("10.1.1.1:80"), "10.1.1.1"},
		{"junk at the right falls back to the peer", reqFrom("10.1.1.1:80", "X-Forwarded-For", "203.0.113.9, not-an-ip"), "10.1.1.1"},
		{"all trusted falls back to the peer", reqFrom("10.1.1.1:80", "X-Forwarded-For", "10.2.2.2"), "10.1.1.1"},
		{"ipv6 client behind ipv6 proxy", reqFrom("[fd00::1]:80", "X-Forwarded-For", "2001:db8::5"), "2001:db8::5"},
		{"mapped ipv4 peer", reqFrom("[::ffff:10.1.1.1]:80", "X-Forwarded-For", "203.0.113.9"), "203.0.113.9"},
		{"mapped ipv4 client", reqFrom("10.1.1.1:80", "X-Forwarded-For", "::ffff:203.0.113.9"), "203.0.113.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := n.ClientIP(tc.r); got.String() != tc.want {
				t.Fatalf("ClientIP = %s, want %s", got, tc.want)
			}
		})
	}
	if got := n.ClientIP(reqFrom("garbage")); got.IsValid() {
		t.Errorf("ClientIP of an unparsable peer = %s, want the zero Addr", got)
	}
}

func TestSchemeSecureAndBaseURL(t *testing.T) {
	proxied, _ := NewNetwork("https://hub.example.com/", false, mustPrefixes(t, "10.0.0.0/8"))
	if proxied.PublicURL() != "https://hub.example.com" || !proxied.Secure() {
		t.Errorf("proxied: PublicURL=%q Secure=%v", proxied.PublicURL(), proxied.Secure())
	}
	if got := proxied.Scheme(reqFrom("10.0.0.1:1", "X-Forwarded-Proto", "https")); got != "https" {
		t.Errorf("trusted X-Forwarded-Proto: %s", got)
	}
	if got := proxied.Scheme(reqFrom("10.0.0.1:1", "X-Forwarded-Proto", "http")); got != "http" {
		t.Errorf("trusted X-Forwarded-Proto http: %s", got)
	}
	if got := proxied.Scheme(reqFrom("10.0.0.1:1", "X-Forwarded-Proto", "gopher")); got != "http" {
		t.Errorf("invalid X-Forwarded-Proto honoured: %s", got)
	}
	if got := proxied.Scheme(reqFrom("203.0.113.1:1", "X-Forwarded-Proto", "https")); got != "http" {
		t.Errorf("untrusted X-Forwarded-Proto honoured: %s", got)
	}
	r := reqFrom("203.0.113.1:1")
	r.TLS = &tls.ConnectionState{}
	if got := proxied.Scheme(r); got != "https" {
		t.Errorf("direct TLS: %s", got)
	}
	if got := proxied.BaseURL(reqFrom("10.0.0.1:1")); got != "https://hub.example.com" {
		t.Errorf("BaseURL with public URL = %s", got)
	}

	plain, _ := NewNetwork("", false, nil)
	if plain.PublicURL() != "" || plain.Secure() {
		t.Errorf("plain: PublicURL=%q Secure=%v", plain.PublicURL(), plain.Secure())
	}
	direct, _ := NewNetwork("", true, nil)
	if !direct.Secure() {
		t.Error("a Hub that terminates TLS must set Secure cookies")
	}
	if got := plain.BaseURL(reqFrom("203.0.113.1:1")); got != "http://hub.test" {
		t.Errorf("BaseURL without public URL = %s", got)
	}
	insecure, _ := NewNetwork("http://10.0.0.5:8080", true, nil)
	if insecure.Secure() {
		t.Error("an http public URL must not set the Secure flag")
	}
}

func TestPackageFunctionsFollowTheRunningHub(t *testing.T) {
	if PublicURL() != "" {
		t.Fatal("PublicURL set without a running Hub")
	}
	if got := ClientIP(reqFrom("10.1.1.1:5", "X-Forwarded-For", "9.9.9.9")); got.String() != "10.1.1.1" {
		t.Fatalf("without a Hub ClientIP trusts a header: %s", got)
	}
	n, _ := NewNetwork("https://hub.example.com", true, mustPrefixes(t, "10.0.0.0/8"))
	setDefaultNetwork(n)
	if PublicURL() != "https://hub.example.com" {
		t.Errorf("PublicURL = %q", PublicURL())
	}
	if got := ClientIP(reqFrom("10.1.1.1:5", "X-Forwarded-For", "9.9.9.9")); got.String() != "9.9.9.9" {
		t.Errorf("ClientIP = %s", got)
	}
	clearDefaultNetwork(n)
	if PublicURL() != "" {
		t.Error("PublicURL still set after the Hub stopped")
	}
}

// ---- health and readiness --------------------------------------------------

func TestHealthEndpointsAndReadinessFlip(t *testing.T) {
	h := NewHealth()
	mux := http.NewServeMux()
	h.Register(mux)
	var dbUp atomic.Bool
	h.AddReadyCheck("database", func(context.Context) error {
		if !dbUp.Load() {
			return fmt.Errorf("secret detail: /var/lib/minion/minion.db is locked")
		}
		return nil
	})

	do := func(path string) (int, string) {
		r, _ := http.NewRequest(http.MethodGet, "http://hub.test"+path, nil)
		rec := &recorder{header: http.Header{}}
		mux.ServeHTTP(rec, r)
		return rec.code, rec.body.String()
	}

	if code, _ := do("/healthz"); code != 200 {
		t.Errorf("/healthz before start = %d, want 200 (liveness)", code)
	}
	if code, body := do("/readyz"); code != 503 || !strings.Contains(body, "starting") {
		t.Errorf("/readyz before start = %d %s", code, body)
	}
	h.MarkStarted()
	code, body := do("/readyz")
	if code != 503 || !strings.Contains(body, `"reason":"database"`) {
		t.Errorf("/readyz with a failing database = %d %s", code, body)
	}
	if strings.Contains(body, "secret detail") || strings.Contains(body, "/var/lib") {
		t.Errorf("/readyz leaks the check error: %s", body)
	}
	dbUp.Store(true)
	if code, body := do("/readyz"); code != 200 || !strings.Contains(body, "ready") {
		t.Errorf("/readyz when ready = %d %s", code, body)
	}
	dbUp.Store(false)
	if code, _ := do("/readyz"); code != 503 {
		t.Errorf("/readyz after the database went away = %d, want 503", code)
	}
	dbUp.Store(true)
	h.MarkStopping()
	if code, body := do("/readyz"); code != 503 || !strings.Contains(body, "shutting_down") {
		t.Errorf("/readyz while stopping = %d %s", code, body)
	}
	if code, _ := do("/healthz"); code != 200 {
		t.Errorf("/healthz while stopping = %d, want 200", code)
	}
}

func TestReadyCheckTimeoutAndPanicFail(t *testing.T) {
	h := NewHealth()
	h.timeout = 20 * time.Millisecond
	h.MarkStarted()
	h.AddReadyCheck("slow", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	if name, ok := h.Ready(context.Background()); ok || name != "slow" {
		t.Fatalf("slow check: %q %v", name, ok)
	}
	h2 := NewHealth()
	h2.MarkStarted()
	h2.AddReadyCheck("boom", func(context.Context) error { panic("x") })
	if name, ok := h2.Ready(context.Background()); ok || name != "boom" {
		t.Fatalf("panicking check: %q %v", name, ok)
	}
}

type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) WriteHeader(c int)   { r.code = c }
func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = 200
	}
	return r.body.Write(b)
}

// ---- data dir (A-14) -------------------------------------------------------

func TestCheckDataDirWritableAndFresh(t *testing.T) {
	dir := t.TempDir()
	if err := CheckDataDir(dir); err != nil {
		t.Fatalf("writable dir: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the probe left files behind: %v", entries)
	}
	fresh := filepath.Join(dir, "not", "yet", "there")
	if err := CheckDataDir(fresh); err != nil {
		t.Fatalf("missing dir should be created: %v", err)
	}
	if fi, err := os.Stat(fresh); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("created dir: %v %v", fi, err)
	}
	if err := CheckDataDir(""); err == nil || !strings.Contains(err.Error(), "MINION_DATA_DIR") {
		t.Errorf("empty dir: %v", err)
	}
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckDataDir(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("file as dir: %v", err)
	}
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission semantics do not apply on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
}

func TestCheckDataDirUnwritablePrintsExactFix(t *testing.T) {
	skipIfRoot(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "data")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := checkDataDir(dir, 65532, 65532)
	if err == nil {
		t.Fatal("unwritable dir accepted")
	}
	want := fmt.Sprintf("data dir %s is not writable by uid 65532; run: chown 65532:65532 <host dir>", dir)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q\nwant it to contain %q", err, want)
	}
	if !strings.Contains(err.Error(), "init container") {
		t.Errorf("error does not mention the init-container equivalent: %v", err)
	}
	// Nothing outside the data dir: the parent path must not appear except as
	// the prefix of the data dir itself.
	if strings.Count(err.Error(), parent) != strings.Count(err.Error(), dir) {
		t.Errorf("error prints a path outside the data dir: %v", err)
	}
}

func TestCheckDataDirMissingAndParentUnwritable(t *testing.T) {
	skipIfRoot(t)
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	dir := filepath.Join(parent, "data")
	err := checkDataDir(dir, 65532, 65532)
	if err == nil {
		t.Fatal("a data dir below an unwritable parent was accepted")
	}
	// A chown cannot fix a directory that does not exist: the fix creates it.
	want := fmt.Sprintf("data dir %s does not exist and uid 65532 cannot create it; run: mkdir -p <host dir> && chown 65532:65532 <host dir> (or mount a host directory at %s, or use an init container)", dir, dir)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q\nwant it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), "is not writable") {
		t.Errorf("a missing dir is reported as unwritable: %v", err)
	}
	if strings.Count(err.Error(), parent) != strings.Count(err.Error(), dir) {
		t.Errorf("error prints a path outside the data dir: %v", err)
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		t.Error("data dir was created in an unwritable parent")
	}
}

// ---- running Hub -----------------------------------------------------------

// testHooks switch the readiness check of the probe module.
type testHooks struct {
	failReady atomic.Bool
}

func registryWith(t *testing.T, withStore bool, hooks *testHooks) *Registry {
	t.Helper()
	r := NewRegistry()
	if err := registerCore(r); err != nil {
		t.Fatal(err)
	}
	if withStore {
		if err := registerStore(r); err != nil {
			t.Fatal(err)
		}
	}
	err := r.Register("probe", 50, func(_ context.Context, env *Env) error {
		mux, err := Require[*http.ServeMux](env)
		if err != nil {
			return err
		}
		nw, err := Require[*Network](env)
		if err != nil {
			return err
		}
		health, err := Require[*Health](env)
		if err != nil {
			return err
		}
		if hooks != nil {
			health.AddReadyCheck("probe", func(context.Context) error {
				if hooks.failReady.Load() {
					return fmt.Errorf("down")
				}
				return nil
			})
		}
		mux.HandleFunc("GET /whoami", func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, "ip=%s scheme=%s secure=%v public=%s fn=%s", nw.ClientIP(r), nw.Scheme(r), nw.Secure(), nw.PublicURL(), ClientIP(r))
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type running struct {
	srv    *Server
	cancel context.CancelFunc
	done   chan error
	logs   *syncBuf
}

func startHub(t *testing.T, cfg *Config, reg *Registry, opts ...Option) *running {
	t.Helper()
	logs := &syncBuf{}
	cfg.Logger = testLogger(logs)
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	srv := New(cfg, reg, opts...)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	select {
	case <-srv.Started():
	case err := <-done:
		cancel()
		t.Fatalf("Run returned before the Hub started: %v\nlogs:\n%s", err, logs)
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("the Hub did not start")
	}
	r := &running{srv: srv, cancel: cancel, done: done, logs: logs}
	t.Cleanup(func() { r.stop(t) })
	return r
}

func (r *running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Errorf("Run returned %v after shutdown", err)
		}
		r.done <- nil // keep stop idempotent
	case <-time.After(20 * time.Second):
		t.Error("the Hub did not stop")
	}
}

func insecureClient(maxVersion uint16) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, MaxVersion: maxVersion}, //nolint:gosec // test client of a self-signed server
		},
	}
}

func getBody(t *testing.T, c *http.Client, url string) (int, string, *http.Response) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp
}

func TestRunServesHealthOverTLS13WithSelfSignedCertificate(t *testing.T) {
	data := t.TempDir()
	reg := registryWith(t, true, nil)
	h := startHub(t, &Config{DataDir: data}, reg)
	base := "https://" + h.srv.Addr()

	c := insecureClient(0)
	if code, _, resp := getBody(t, c, base+"/healthz"); code != 200 || resp.TLS.Version != tls.VersionTLS13 {
		t.Fatalf("/healthz = %d, TLS version %#x", code, resp.TLS.Version)
	}
	code, body, resp := getBody(t, c, base+"/readyz")
	if code != 200 {
		t.Fatalf("/readyz = %d %s (database open and migrations applied)", code, body)
	}

	// The certificate the client saw is the generated one, and the log says
	// its fingerprint exactly once.
	fp := CertFingerprint(resp.TLS.PeerCertificates[0].Raw)
	logs := h.logs.String()
	if n := strings.Count(logs, fp); n != 1 {
		t.Errorf("the fingerprint %s appears %d times in the log, want once:\n%s", fp, n, logs)
	}
	if n := strings.Count(logs, "tls certificate"); n != 1 {
		t.Errorf("tls certificate log lines = %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(data, TLSDir, TLSCertFile)); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(data, "minion.db")); err != nil {
		t.Error("the store did not open the database:", err)
	}

	// A TLS 1.2 client is rejected.
	old := insecureClient(tls.VersionTLS12)
	if resp, err := old.Get(base + "/healthz"); err == nil {
		resp.Body.Close()
		t.Fatal("a TLS 1.2 client was served")
	}
	// Plain HTTP on the TLS port is not served either.
	if resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + h.srv.Addr() + "/healthz"); err == nil {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatal("plain HTTP served on the TLS port")
		}
	}
}

func TestRunReusesCertificateAcrossRestarts(t *testing.T) {
	data := t.TempDir()
	fps := make([]string, 2)
	for i := range fps {
		h := startHub(t, &Config{DataDir: data}, registryWith(t, false, nil))
		_, _, resp := getBody(t, insecureClient(0), "https://"+h.srv.Addr()+"/healthz")
		fps[i] = CertFingerprint(resp.TLS.PeerCertificates[0].Raw)
		h.stop(t)
		if i == 1 && !strings.Contains(h.logs.String(), `generated=false`) {
			t.Errorf("second start log does not say the certificate was reused:\n%s", h.logs)
		}
	}
	if fps[0] != fps[1] {
		t.Fatal("the certificate changed after a restart")
	}
}

func TestRunWithOwnCertificateReloadsOnSignal(t *testing.T) {
	cert, key := ownPair(t, "127.0.0.1")
	reload := make(chan struct{}, 1)
	h := startHub(t, &Config{DataDir: t.TempDir(), CertFile: cert, KeyFile: key}, registryWith(t, false, nil), WithReloadSignal(reload))
	url := "https://" + h.srv.Addr() + "/healthz"

	seen := func() string {
		_, _, resp := getBody(t, insecureClient(0), url)
		return CertFingerprint(resp.TLS.PeerCertificates[0].Raw)
	}
	want, _ := LoadCertSource(CertOptions{CertFile: cert, KeyFile: key})
	first := seen()
	if first != want.Fingerprint() {
		t.Fatal("the Hub does not serve the configured certificate")
	}

	newCert, newKey := ownPair(t, "127.0.0.1")
	copyFile(t, newCert, cert)
	copyFile(t, newKey, key)
	reload <- struct{}{}
	deadline := time.Now().Add(10 * time.Second)
	for seen() == first {
		if time.Now().After(deadline) {
			t.Fatalf("the certificate was not reloaded\nlogs:\n%s", h.logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(h.logs.String(), "tls certificate reloaded") {
		t.Errorf("no reload log line:\n%s", h.logs)
	}
}

func TestRunPlainHTTPWithTrustedProxy(t *testing.T) {
	cfg := &Config{
		DataDir:        t.TempDir(),
		TLSOff:         true,
		PublicURL:      "https://hub.example.com",
		TrustedProxies: mustPrefixes(t, "127.0.0.0/8, ::1"),
	}
	h := startHub(t, cfg, registryWith(t, true, nil))
	base := "http://" + h.srv.Addr()

	if _, err := os.Stat(filepath.Join(cfg.DataDir, TLSDir)); !os.IsNotExist(err) {
		t.Error("TLS off must not generate a certificate")
	}
	if strings.Contains(h.logs.String(), "tls certificate") {
		t.Error("TLS off logged a certificate fingerprint")
	}
	if code, _, _ := getBody(t, http.DefaultClient, base+"/healthz"); code != 200 {
		t.Fatalf("/healthz = %d", code)
	}
	if code, _, _ := getBody(t, http.DefaultClient, base+"/readyz"); code != 200 {
		t.Fatalf("/readyz = %d", code)
	}

	req, _ := http.NewRequest(http.MethodGet, base+"/whoami", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.77")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	got := string(b)
	for _, want := range []string{"ip=203.0.113.77", "scheme=https", "secure=true", "public=https://hub.example.com", "fn=203.0.113.77"} {
		if !strings.Contains(got, want) {
			t.Errorf("/whoami = %q, missing %q", got, want)
		}
	}
}

func TestRunIgnoresForwardedHeadersFromUntrustedPeers(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir(), TLSOff: true, PublicURL: "http://hub.example.com"} // no trusted proxies
	h := startHub(t, cfg, registryWith(t, false, nil))
	req, _ := http.NewRequest(http.MethodGet, "http://"+h.srv.Addr()+"/whoami", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.77")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	got := string(b)
	if strings.Contains(got, "203.0.113.77") || strings.Contains(got, "scheme=https") {
		t.Fatalf("forwarded headers honoured from an untrusted peer: %q", got)
	}
	if !strings.Contains(got, "ip=127.0.0.1") || !strings.Contains(got, "scheme=http") {
		t.Fatalf("/whoami = %q", got)
	}
}

func TestRunReadinessFlipsAndShutdownDrains(t *testing.T) {
	hooks := &testHooks{}
	h := startHub(t, &Config{DataDir: t.TempDir(), TLSOff: true, PublicURL: "http://hub.example.com"}, registryWith(t, true, hooks))
	url := "http://" + h.srv.Addr() + "/readyz"
	if code, _, _ := getBody(t, http.DefaultClient, url); code != 200 {
		t.Fatalf("/readyz = %d", code)
	}
	hooks.failReady.Store(true)
	if code, body, _ := getBody(t, http.DefaultClient, url); code != 503 || !strings.Contains(body, "probe") {
		t.Fatalf("/readyz with a failing module check = %d %s", code, body)
	}
	hooks.failReady.Store(false)
	if code, _, _ := getBody(t, http.DefaultClient, url); code != 200 {
		t.Fatalf("/readyz after recovery = %d", code)
	}
	h.stop(t)
	if _, err := http.Get(url); err == nil {
		t.Error("the Hub still answers after shutdown")
	}
	if PublicURL() != "" {
		t.Error("the package-level public URL outlived the Hub")
	}
}

func TestRunRefusesUnwritableDataDirBeforeOpeningAnyFile(t *testing.T) {
	skipIfRoot(t)
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	srv := New(&Config{DataDir: dir, Listen: "127.0.0.1:0", Logger: testLogger(io.Discard)}, registryWith(t, true, nil))
	err := srv.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is not writable by uid") || !strings.Contains(err.Error(), "<host dir>") {
		t.Fatalf("Run = %v", err)
	}
	select {
	case <-srv.Started():
		t.Fatal("the Hub started")
	default:
	}
	_ = os.Chmod(dir, 0o700)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("files were created in the unwritable data dir: %v", entries)
	}
}

func TestRunStartsInFreshDataDir(t *testing.T) {
	data := filepath.Join(t.TempDir(), "fresh")
	h := startHub(t, &Config{DataDir: data}, registryWith(t, true, nil))
	if code, _, _ := getBody(t, insecureClient(0), "https://"+h.srv.Addr()+"/readyz"); code != 200 {
		t.Fatalf("/readyz in a fresh data dir = %d", code)
	}
}

func TestRunFailsWhenAddressIsTaken(t *testing.T) {
	h := startHub(t, &Config{DataDir: t.TempDir(), TLSOff: true, PublicURL: "http://h.example"}, registryWith(t, false, nil))
	srv := New(&Config{DataDir: t.TempDir(), TLSOff: true, PublicURL: "http://h.example", Listen: h.srv.Addr(), Logger: testLogger(io.Discard)}, registryWith(t, false, nil))
	err := srv.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "MINION_LISTEN") {
		t.Fatalf("Run = %v", err)
	}
}

func TestCoreModuleNeedsServerRun(t *testing.T) {
	r := NewRegistry()
	if err := registerCore(r); err != nil {
		t.Fatal(err)
	}
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Server.Run") {
		t.Fatalf("Start = %v", err)
	}
}

func TestDefaultRegistryHasCoreThenStore(t *testing.T) {
	names := DefaultRegistry.Names()
	if len(names) < 2 || names[0] != "core" || names[1] != "store" {
		t.Fatalf("default modules = %v, want core then store first", names)
	}
}
