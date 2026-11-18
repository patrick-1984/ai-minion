package syslog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// readOctetFrames reads RFC 6587 octet-counted frames from r into out.
func readOctetFrames(r io.Reader, out chan<- []byte) {
	br := bufio.NewReader(r)
	for {
		lenText, err := br.ReadString(' ')
		if err != nil {
			close(out)
			return
		}
		n, err := strconv.Atoi(strings.TrimSuffix(lenText, " "))
		if err != nil || n <= 0 {
			close(out)
			return
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			close(out)
			return
		}
		out <- buf
	}
}

func msgJSON(t *testing.T, m []byte) map[string]any {
	t.Helper()
	i := bytes.Index(m, bom)
	if i < 0 {
		t.Fatalf("no BOM in %q", m)
	}
	var obj map[string]any
	if err := json.Unmarshal(m[i+3:], &obj); err != nil {
		t.Fatalf("MSG is not JSON: %q", m)
	}
	return obj
}

func recv(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case m, ok := <-ch:
		if !ok {
			t.Fatal("the receiver stopped")
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}
	return nil
}

func TestTCPOctetCountedFrames(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	frames := make(chan []byte, 10)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			readOctetFrames(c, frames)
		}
	}()
	w := NewWriter(Options{Network: "tcp", Addr: ln.Addr().String()})
	f := &Formatter{Hostname: "h", AppName: "minion-hub", ProcID: "1", MaxBytes: 2048}
	log := slog.New(NewHandler(f, w)).With("component", "workers")
	log.Info("line one\nwith newline", "worker", "w1", "token", "s3cr3t")
	log.WithGroup("g").Warn("two", "a", 1)
	m1, m2 := recv(t, frames), recv(t, frames)
	if !bytes.HasPrefix(m1, []byte("<134>1 ")) || !bytes.Contains(m1, []byte(" h minion-hub 1 workers - ")) {
		t.Fatalf("header: %q", m1)
	}
	o1 := msgJSON(t, m1)
	if o1["msg"] != "line one\nwith newline" || o1["worker"] != "w1" || o1["token"] != "[redacted]" || o1["component"] != "workers" {
		t.Fatalf("msg 1 = %v", o1)
	}
	o2 := msgJSON(t, m2)
	if g, _ := o2["g"].(map[string]any); g == nil || g["a"] != float64(1) {
		t.Fatalf("msg 2 = %v", o2)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := w.Stats(); s.Sent != 2 || s.LastSuccess.IsZero() {
		t.Fatalf("stats = %+v", s)
	}
}

type pki struct {
	caPool         *x509.CertPool
	server, client tls.Certificate
	caCert         *x509.Certificate
	caKey          *ecdsa.PrivateKey
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	p := &pki{caPool: x509.NewCertPool(), caCert: ca, caKey: caKey}
	p.caPool.AddCert(ca)
	p.server = p.leaf(t, 2, x509.ExtKeyUsageServerAuth)
	p.client = p.leaf(t, 3, x509.ExtKeyUsageClientAuth)
	return p
}

func (p *pki) leaf(t *testing.T, serial int64, usage x509.ExtKeyUsage) tls.Certificate {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, p.caCert, &k.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

func TestTLSWithRequiredClientCertificate(t *testing.T) {
	p := newPKI(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{p.server},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: p.caPool, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	frames := make(chan []byte, 10)
	var peers sync.Map
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				tc := c.(*tls.Conn)
				if tc.Handshake() != nil {
					_ = c.Close()
					return
				}
				peers.Store(tc.ConnectionState().PeerCertificates[0].SerialNumber.Int64(), true)
				readOctetFrames(c, frames)
			}()
		}
	}()
	f := &Formatter{Hostname: "h", AppName: "minion-worker", ProcID: "9", MaxBytes: 2048}

	// Without a client certificate the server refuses: nothing arrives.
	var diag sync.Map
	bad := NewWriter(Options{Network: "tls", Addr: ln.Addr().String(), BackoffMin: 50 * time.Millisecond,
		TLS:  &tls.Config{RootCAs: p.caPool, ServerName: "localhost"},
		Diag: func(class string, err error) { diag.Store(class, err) }})
	slog.New(NewHandler(f, bad)).Info("refused")
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	_ = bad.Close(ctx)
	cancel()
	// (With TLS 1.3 the client may finish its handshake and write before the
	// server rejects it; what counts is that nothing arrives.)
	select {
	case m := <-frames:
		t.Fatalf("a message arrived without a client certificate: %q", m)
	default:
	}
	if _, ok := peers.Load(int64(3)); ok {
		t.Fatal("unexpected peer")
	}

	w := NewWriter(Options{Network: "tls", Addr: ln.Addr().String(),
		TLS: &tls.Config{RootCAs: p.caPool, ServerName: "localhost", Certificates: []tls.Certificate{p.client}}})
	slog.New(NewHandler(f, w)).Error("over mtls", "n", 2)
	m := recv(t, frames)
	if !bytes.HasPrefix(m, []byte("<131>1 ")) || msgJSON(t, m)["msg"] != "over mtls" {
		t.Fatalf("got %q", m)
	}
	if _, ok := peers.Load(int64(3)); !ok {
		t.Fatal("the server did not see the client certificate")
	}
	_ = w.Close(context.Background())
}

func TestUDPDatagramWithinBudget(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	w := NewWriter(Options{Network: "udp", Addr: pc.LocalAddr().String()})
	f := &Formatter{Hostname: "h", AppName: "a", ProcID: "1", MaxBytes: 480}
	slog.New(NewHandler(f, w)).Info(strings.Repeat("x", 1000), "big", strings.Repeat("y", 900))
	buf := make([]byte, 65536)
	_ = pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n > 480 || bytes.HasPrefix(buf, []byte("4")) {
		t.Fatalf("datagram of %d bytes (no octet count on UDP): %q", n, buf[:20])
	}
	if o := msgJSON(t, buf[:n]); o["truncated"] != true {
		t.Fatalf("not reduced: %v", o)
	}
	_ = w.Close(context.Background())
}

// A peer that accepts and never reads must not block the logger, and Close
// honours its deadline.
func TestStalledPeerNeverBlocksTheLogger(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // never read
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		mu.Unlock()
	}()
	w := NewWriter(Options{Network: "tcp", Addr: ln.Addr().String(), WriteTimeout: 300 * time.Millisecond, BackoffMin: 50 * time.Millisecond})
	f := &Formatter{Hostname: "h", AppName: "a", ProcID: "1", MaxBytes: 65000}
	log := slog.New(NewHandler(f, w))
	big := strings.Repeat("z", 1900)
	const offered = 5000
	start := time.Now()
	for range offered {
		log.Info("x", "v", big)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("logging blocked for %s", d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start = time.Now()
	_ = w.Close(ctx)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %s", d)
	}
	s := w.Stats()
	var dropped uint64
	for _, n := range s.Dropped {
		dropped += n
	}
	if s.Queued != 0 || s.Sent+dropped != offered {
		t.Fatalf("stats do not add up: %+v", s)
	}
}

// Nothing listens: the writer backs off, the queue drops the oldest, logging
// stays non-blocking, Discard counts the queue as reconfigure.
func TestDeadEndpointAndDiscard(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	var mu sync.Mutex
	classes := map[string]int{}
	w := NewWriter(Options{Network: "tcp", Addr: addr, QueueSize: 10, BackoffMin: 20 * time.Millisecond, BackoffMax: 40 * time.Millisecond,
		Diag: func(class string, err error) {
			mu.Lock()
			classes[class]++
			mu.Unlock()
		}})
	f := &Formatter{Hostname: "h", AppName: "a", ProcID: "1", MaxBytes: 2048}
	log := slog.New(NewHandler(f, w))
	start := time.Now()
	for range 100 {
		log.Info("x")
	}
	if time.Since(start) > time.Second {
		t.Fatal("logging blocked")
	}
	time.Sleep(150 * time.Millisecond)
	w.Discard()
	s := w.Stats()
	mu.Lock()
	defer mu.Unlock()
	if classes["connect"] == 0 || s.LastError == "" {
		t.Fatalf("no connect diagnostics: %v %+v", classes, s)
	}
	total := s.Dropped[DropQueueFull] + s.Dropped[DropReconfigure] + s.Dropped[DropShutdown]
	if s.Dropped[DropQueueFull] < 89 || s.Dropped[DropReconfigure] == 0 || total != 100 {
		t.Fatalf("dropped = %v", s.Dropped)
	}
}
