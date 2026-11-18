package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func wsEcho() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}})
		if err == nil {
			defer c.CloseNow()
			time.Sleep(testWait / 5)
		}
	})
}

func dialSeen(t *testing.T, url string) TLSSeen {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	conn, facts, err := DialInfo(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	return facts.TLS
}

func TestTLSSeenSelfSigned(t *testing.T) {
	cert := testCert(t, "hub.lan", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := httptest.NewUnstartedServer(wsEcho())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()
	seen := dialSeen(t, srv.URL)
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	if seen.None || seen.KeyHash != sha256.Sum256(leaf.RawSubjectPublicKeyInfo) || seen.Name != "hub.lan" ||
		!seen.SelfSigned || seen.Trusted || seen.Reason == "" || !seen.Seen() {
		t.Fatalf("seen = %+v", seen)
	}
}

// trustedServer starts a TLS server for 127.0.0.1 whose certificate a test CA
// issued, and makes this process trust that CA for the display verification.
func trustedServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "hub"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	trustRoots = pool
	t.Cleanup(func() { trustRoots = nil })
	return srv
}

func TestTLSSeenTrustedByATestCA(t *testing.T) {
	srv := trustedServer(t, wsEcho())
	seen := dialSeen(t, srv.URL)
	if !seen.Trusted || seen.SelfSigned || seen.Issuer != "Test CA" || seen.Name != "127.0.0.1" || seen.Reason != "" {
		t.Fatalf("seen = %+v", seen)
	}
}

func dialFacts(t *testing.T, url string) DialFacts {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	conn, facts, err := DialInfo(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	return facts
}

// Only an upgrade answered directly by the dialled host over a verified TLS
// connection counts for an admission; a redirect, even to a host with a
// trusted certificate, never does.
func TestDirectVerifiedOnlyForTheDialledHostWithoutRedirect(t *testing.T) {
	target := trustedServer(t, wsEcho())
	if f := dialFacts(t, target.URL); !f.DirectVerified || f.Redirected {
		t.Fatalf("direct: %+v", f)
	}
	redirector := trustedServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	f := dialFacts(t, redirector.URL)
	if f.DirectVerified || !f.Redirected {
		t.Fatalf("redirected: %+v", f)
	}
	plain := httptest.NewServer(wsEcho())
	defer plain.Close()
	if f := dialFacts(t, plain.URL); f.DirectVerified {
		t.Fatalf("plain: %+v", f)
	}
}

func TestDirectVerifiedRefusesAProxyAndAnotherHost(t *testing.T) {
	ok := DialFacts{TLS: TLSSeen{KeyHash: [32]byte{1}, Trusted: true}}
	u := func(s string) *url.URL { v, _ := url.Parse(s); return v }
	if !directVerified(ok, "wss://hub.example:8443/x", u("https://hub.example:8443/x"), true) {
		t.Fatal("the direct case")
	}
	proxied := ok
	proxied.Proxy = "http://proxy.lan:3128"
	for name, got := range map[string]bool{
		"proxy":        directVerified(proxied, "wss://hub.example:8443/x", u("https://hub.example:8443/x"), true),
		"another host": directVerified(ok, "wss://hub.example:8443/x", u("https://other.example:8443/x"), true),
		"plain ws":     directVerified(ok, "ws://hub.example/x", u("http://hub.example/x"), true),
		"no tls state": directVerified(ok, "wss://hub.example/x", u("https://hub.example/x"), false),
		"untrusted":    directVerified(DialFacts{TLS: TLSSeen{KeyHash: [32]byte{1}}}, "wss://hub.example/x", u("https://hub.example/x"), true),
	} {
		if got {
			t.Errorf("%s counted as direct", name)
		}
	}
}

func TestTLSSeenPlain(t *testing.T) {
	srv := httptest.NewServer(wsEcho())
	defer srv.Close()
	if seen := dialSeen(t, srv.URL); !seen.None || !seen.Seen() {
		t.Fatalf("seen = %+v", seen)
	}
}

func TestTLSSeenNamesAreOneCleanLine(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	hostile := "evil\nINJECTED\r\x1b[2Aline\u202e\u200b"
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: hostile},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	seen := seeTLS(&tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}, "hub.lan")
	for _, s := range []string{seen.Name, seen.Issuer, seen.Reason} {
		if strings.ContainsAny(s, "\n\r\x1b\u202e\u200b") {
			t.Fatalf("unsafe text %q", s)
		}
	}
	if !strings.Contains(seen.Name, "INJECTED") {
		t.Fatalf("name %q", seen.Name)
	}
}

func TestCertReason(t *testing.T) {
	if CertReason(x509.UnknownAuthorityError{}) != "signed by an unknown authority" ||
		CertReason(x509.CertificateInvalidError{Reason: x509.Expired}) != "expired or not yet valid" ||
		!strings.HasPrefix(CertReason(x509.HostnameError{Certificate: &x509.Certificate{DNSNames: []string{"a.example"}}, Host: "b"}), "issued for other names (a.example)") ||
		CertReason(nil) != "" {
		t.Fatal("reasons")
	}
}
