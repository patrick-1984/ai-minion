package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // RFC 6455 defines the accept key with SHA-1; no security use
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/transport"
)

// The proxy is part of the container job's second variant: it must terminate
// TLS with a certificate a client can verify for localhost, forward plain
// requests and WebSocket upgrades (the Worker's connection is one), and set
// the forwarding headers the Hub trusts from it.
func TestProxyForwardsRequestsAndWebSocketsOverVerifiedTLS(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen["proto"], seen["for"], seen["host"] = r.Header.Get("X-Forwarded-Proto"), r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Host")
		mu.Unlock()
		if r.URL.Path == "/ws" {
			// The Worker's own server side: it demands subprotocol minion.v1.
			c, err := transport.Accept(w, r)
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
			msg := make([]byte, 4)
			if _, err := io.ReadFull(c, msg); err != nil {
				return
			}
			_, _ = c.Write(append([]byte("echo:"), msg...))
			return
		}
		_, _ = w.Write([]byte("hub"))
	}))
	defer upstream.Close()
	up, _ := url.Parse(upstream.URL)

	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, up, dir) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()

	var pemBytes []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pemBytes, err = os.ReadFile(filepath.Join(dir, "cert.pem")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(pemBytes) == 0 {
		t.Fatal("the proxy wrote no certificate")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("cert.pem is not a certificate")
	}
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13},
	}}
	base := "localhost:" + ln.Addr().(*net.TCPAddr).AddrPort().String()[len("127.0.0.1:"):]

	resp, err := hc.Get("https://" + base + "/")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	_ = resp.Body.Close()
	mu.Lock()
	if seen["proto"] != "https" || seen["for"] != "127.0.0.1" || seen["host"] != base {
		t.Errorf("the Hub would see X-Forwarded-Proto=%q For=%q Host=%q", seen["proto"], seen["for"], seen["host"])
	}
	mu.Unlock()

	// A client that does not trust the certificate must be refused: the proxy
	// presents a real certificate, not "any".
	if r, err := (&http.Client{Timeout: 10 * time.Second}).Get("https://" + base + "/"); err == nil {
		_ = r.Body.Close()
		t.Fatal("a client without the certificate connected")
	}

	// The WebSocket upgrade goes over the same verified TLS config (owned CA,
	// name localhost, TLS 1.3); transport.Dial does not verify TLS on purpose
	// (A-15), so the client side is a fixed RFC 6455 exchange on a stdlib TLS
	// connection.
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", base,
		&tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatalf("TLS to the proxy for the WebSocket: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	const key = "dGhlIHNhbXBsZSBub25jZQ==" // RFC 6455 section 1.3 sample nonce
	req := "GET /ws HTTP/1.1\r\nHost: " + base + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: " + transport.Subprotocol + "\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	up2, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("WebSocket upgrade through the proxy: %v", err)
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) //nolint:gosec // see import
	if up2.StatusCode != http.StatusSwitchingProtocols ||
		up2.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) ||
		up2.Header.Get("Sec-WebSocket-Protocol") != transport.Subprotocol {
		t.Fatalf("upgrade = %d accept=%q protocol=%q", up2.StatusCode, up2.Header.Get("Sec-WebSocket-Accept"), up2.Header.Get("Sec-WebSocket-Protocol"))
	}
	// One masked binary frame carrying "ping" (mask 01 02 03 04), then the
	// server's unmasked binary frame carrying "echo:ping".
	mask := []byte{1, 2, 3, 4}
	payload := []byte("ping")
	frame := append([]byte{0x82, 0x80 | byte(len(payload))}, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
	want := append([]byte{0x82, 9}, "echo:ping"...)
	got := make([]byte, len(want))
	if _, err := io.ReadFull(br, got); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("echo frame = %x, %v; want %x", got, err, want)
	}
}
