// Command proxy is the reverse proxy of the second variant of the container
// end-to-end job (Story 9.6): it terminates TLS with a certificate it
// generates for localhost, forwards every request, WebSocket upgrades included,
// to one upstream Hub that runs with MINION_TLS=off, and sets the X-Forwarded-*
// headers the Hub honours only from this proxy's address
// (MINION_TRUSTED_PROXIES). It exists only for the test: it is not shipped.
package main

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
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	listen := flag.String("listen", ":8443", "address to listen on (TLS)")
	upstream := flag.String("upstream", "", "base URL of the plain-HTTP Hub, for example http://hub-proxied:8443")
	certDir := flag.String("cert-dir", "", "directory that receives cert.pem (the certificate clients must trust)")
	flag.Parse()
	if err := run(*listen, *upstream, *certDir); err != nil {
		log.Fatalf("proxy: %v", err)
	}
}

func run(listen, upstream, certDir string) error {
	up, err := url.Parse(upstream)
	if err != nil || up.Scheme != "http" || up.Host == "" {
		return fmt.Errorf("-upstream must be an http URL, got %q", upstream)
	}
	if certDir == "" {
		return errors.New("-cert-dir is required")
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, ln, up, certDir)
}

// serve proxies TLS connections accepted on ln to up until ctx ends. The
// certificate it presents is written to certDir/cert.pem before it serves.
func serve(ctx context.Context, ln net.Listener, up *url.URL, certDir string) error {
	cert, certPEM, err := selfSigned()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(certDir, "cert.pem"), certPEM, 0o644); err != nil {
		return err
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(up)
			r.SetXForwarded() // the Hub trusts these only from this proxy's address
		},
		FlushInterval: -1, // stream server-sent events at once
	}
	srv := &http.Server{
		Handler:           rp,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13},
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("proxy: listening on %s, upstream %s", ln.Addr(), up)
	if err := srv.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// selfSigned returns a certificate for localhost, 127.0.0.1 and ::1 (the names
// the test client verifies) and its PEM.
func selfSigned() (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Minion e2e proxy"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}
