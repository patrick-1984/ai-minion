package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func leafPin(srv *httptest.Server) string {
	return identity.SPKIPin(srv.Certificate().RawSubjectPublicKeyInfo)
}

// otherPin is a well-formed pin of no key used here.
const otherPin = "sha256//47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="

func pinnedHub(t *testing.T, f *hubFixture, pin string) *HubBackend {
	t.Helper()
	cfg := f.cfg()
	cfg.CAFile = ""
	cfg.TLSPin = pin
	b, err := NewHub(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}

func TestHubTLSPinAcceptsExactlyThePinnedKey(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, `{"ok":true}`) })
	pin := leafPin(f.srv)
	// One pin, and the same pin in a list with spaces: the chain is not
	// verified (the httptest certificate is self-signed for other names).
	for _, v := range []string{pin, otherPin + " ; " + pin} {
		if _, err := pinnedHub(t, f, v).Call(context.Background(), ops.OpSessionsList, nil); err != nil {
			t.Errorf("pin %q: %v", v, err)
		}
	}
	// Also for a host name the certificate does not name.
	cfg := f.cfg()
	cfg.CAFile, cfg.TLSPin = "", pin
	_, port, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
	cfg.URL = "https://localhost:" + port
	b, err := NewHub(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call(context.Background(), ops.OpSessionsList, nil); err != nil {
		t.Errorf("pinned, other host name: %v", err)
	}
}

func TestHubTLSPinMismatchIsAHardErrorThatShowsBothValues(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, `{}`) })
	_, err := pinnedHub(t, f, otherPin).Call(context.Background(), ops.OpSendText, map[string]any{"request_id": "r"})
	var ce *HubCertError
	if !errors.Is(err, ErrHubCertificate) || !errors.As(err, &ce) || errors.Is(err, ErrConnectionLost) {
		t.Fatalf("error = %v", err)
	}
	if ce.Pin != leafPin(f.srv) || len(ce.Expected) != 1 || ce.Expected[0] != otherPin {
		t.Errorf("error fields = %+v", ce)
	}
	if msg := err.Error(); !strings.Contains(msg, otherPin) || !strings.Contains(msg, leafPin(f.srv)) || !strings.Contains(msg, "minion hub identity --words") {
		t.Errorf("the error does not show both values: %v", err)
	}
	if f.hits.Load() != 0 {
		t.Error("a request reached a Hub whose key did not match the pin")
	}
}

func TestHubTLSPinMalformedOrCombinedIsRefusedAtStart(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {})
	for _, v := range []string{
		"CANARY",
		"sha256/" + strings.TrimPrefix(otherPin, "sha256//"),
		"sha1//47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=",
		"sha256//AAAA",
		otherPin + ";",
		otherPin + ";;" + otherPin,
		";",
	} {
		cfg := f.cfg()
		cfg.CAFile, cfg.TLSPin = "", v
		_, err := NewHub(cfg)
		if !errors.Is(err, ErrHubTLSPin) || !strings.Contains(err.Error(), "--hub-tls-pin") {
			t.Errorf("%q: error = %v", v, err)
		}
	}
	cfg := f.cfg()
	cfg.TLSPin = otherPin // and the CA file
	if _, err := NewHub(cfg); !errors.Is(err, ErrHubTLSConflict) {
		t.Errorf("pin + CA: %v", err)
	}
	cfg.CAFile, cfg.Insecure = "", true
	if _, err := NewHub(cfg); !errors.Is(err, ErrHubTLSConflict) {
		t.Errorf("pin + insecure: %v", err)
	}
}

// certErrOf calls a Hub and returns its certificate error.
func certErrOf(t *testing.T, cfg HubConfig) *HubCertError {
	t.Helper()
	b, err := NewHub(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_, err = b.Call(context.Background(), ops.OpSessionsList, nil)
	var ce *HubCertError
	if !errors.As(err, &ce) || !errors.Is(err, ErrHubCertificate) {
		t.Fatalf("error = %v", err)
	}
	return ce
}

func checkCertMessage(t *testing.T, err error, reason, pin string) {
	t.Helper()
	msg := err.Error()
	for _, want := range []string{reason, "Its key pin is " + pin, "--hub-tls-pin", "--hub-ca-file", "tls/cert.pem", "minion hub identity --words", "Compare it"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error lacks %q: %s", want, msg)
		}
	}
}

func TestHubCertificateErrorNamesTheReasonAndThePresentedPin(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, `{}`) })
	pin := leafPin(f.srv)

	t.Run("unknown authority", func(t *testing.T) {
		cfg := f.cfg()
		cfg.CAFile = ""
		ce := certErrOf(t, cfg)
		if ce.Pin != pin || ce.Expected != nil {
			t.Errorf("fields = %+v", ce)
		}
		checkCertMessage(t, ce, "unknown authority", pin)
	})
	t.Run("other names", func(t *testing.T) {
		cfg := f.cfg()
		_, port, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
		cfg.URL = "https://localhost:" + port
		ce := certErrOf(t, cfg)
		checkCertMessage(t, ce, "issued for other names", pin)
	})
	t.Run("expired", func(t *testing.T) {
		srv, certPEM, leaf := expiredServer(t)
		cfg := f.cfg()
		cfg.URL = srv.URL
		cfg.CAFile = writeFile(t, t.TempDir(), "expired.pem", certPEM)
		ce := certErrOf(t, cfg)
		checkCertMessage(t, ce, "expired or not yet valid", identity.SPKIPin(leaf.RawSubjectPublicKeyInfo))
	})
	if f.hits.Load() != 0 {
		t.Errorf("a request reached an untrusted Hub")
	}
}

// expiredServer is a TLS server whose self-signed certificate for 127.0.0.1
// expired an hour ago.
func expiredServer(t *testing.T) (*httptest.Server, []byte, *x509.Certificate) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(9), Subject: pkix.Name{CommonName: "expired hub"},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, `{}`) }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), leaf
}

// A TLS pin or a CA file is checked only in a TLS handshake: with a plain
// http URL (also loopback) the client is refused before any request, so the
// bearer key never reaches a listener that proved nothing.
func TestHubTLSPinOrCARefusesPlainHTTPEvenOnLoopback(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {})
	var hits int
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer plain.Close()
	for _, tc := range []struct{ name, pin, ca string }{
		{"pin", otherPin, ""},
		{"ca", "", f.cfg().CAFile},
	} {
		cfg := f.cfg()
		cfg.URL, cfg.TLSPin, cfg.CAFile = plain.URL, tc.pin, tc.ca
		b, err := NewHub(cfg)
		if err == nil {
			_, _ = b.Call(context.Background(), ops.OpSessionsList, nil)
			b.Close()
			t.Fatalf("%s: a plain http URL was accepted", tc.name)
		}
		if !errors.Is(err, ErrHubURL) || !strings.Contains(err.Error(), "not https") {
			t.Errorf("%s: error = %v", tc.name, err)
		}
	}
	if hits != 0 {
		t.Errorf("the plain http listener received %d requests", hits)
	}
}
