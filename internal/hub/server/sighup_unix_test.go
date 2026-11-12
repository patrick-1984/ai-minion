//go:build unix

package server

import (
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunReloadsCertificateOnRealSIGHUP covers the real signal path: no
// WithReloadSignal override, a real SIGHUP to this process.
func TestRunReloadsCertificateOnRealSIGHUP(t *testing.T) {
	cert, key := ownPair(t, "127.0.0.1")
	h := startHub(t, &Config{DataDir: hubstore.Dir(t), CertFile: cert, KeyFile: key}, registryWith(t, false, nil))
	url := "https://" + h.srv.Addr() + "/healthz"
	seen := func() string {
		_, _, resp := getBody(t, insecureClient(0), url)
		return CertFingerprint(resp.TLS.PeerCertificates[0].Raw)
	}
	first := seen()

	newCert, newKey := ownPair(t, "127.0.0.1")
	copyFile(t, newCert, cert)
	copyFile(t, newKey, key)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for seen() == first {
		if time.Now().After(deadline) {
			t.Fatalf("the certificate was not reloaded after SIGHUP\nlogs:\n%s", h.logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(h.logs.String(), "tls certificate reloaded") {
		t.Errorf("no reload log line:\n%s", h.logs)
	}
}
