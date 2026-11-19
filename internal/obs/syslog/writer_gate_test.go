package syslog

import (
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Concurrent producers on a full queue never overwrite a live entry or push
// the count past the capacity; every message is queued, in hand or dropped.
func TestConcurrentEnqueueKeepsTheQueueConsistent(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	w := NewWriter(Options{Network: "tcp", Addr: addr, QueueSize: 16, BackoffMin: time.Hour, BackoffMax: time.Hour})
	var wg sync.WaitGroup
	const producers, each = 8, 2000
	for p := range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				w.Enqueue([]byte(strconv.Itoa(p) + "-" + strconv.Itoa(i)))
			}
		}()
	}
	wg.Wait()
	w.mu.Lock()
	n := w.n
	live := 0
	for i := 0; i < w.n; i++ {
		if len(w.queue[(w.head+i)%len(w.queue)]) > 0 {
			live++
		}
	}
	w.mu.Unlock()
	s := w.Stats()
	if n > 16 || live != n {
		t.Fatalf("queue count %d, live entries %d", n, live)
	}
	// One message may be in the writer's hand (waiting for a connection).
	if total := uint64(n) + s.Dropped[DropQueueFull]; total != producers*each && total != producers*each-1 {
		t.Fatalf("queued %d + dropped %d != %d", n, s.Dropped[DropQueueFull], producers*each)
	}
	w.Discard()
}

// A peer that accepts and resets at once costs a backoff per reconnect,
// not a busy loop of connections.
func TestWriteFailuresBackOff(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var conns atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0) // reset
			}
			_ = c.Close()
		}
	}()
	w := NewWriter(Options{Network: "tcp", Addr: ln.Addr().String(), BackoffMin: 200 * time.Millisecond, BackoffMax: time.Second})
	stop := time.After(1200 * time.Millisecond)
loop:
	for {
		select {
		case <-stop:
			break loop
		default:
			w.Enqueue([]byte(strings.Repeat("x", 4096)))
			time.Sleep(time.Millisecond)
		}
	}
	w.Discard()
	if n := conns.Load(); n > 8 {
		t.Fatalf("%d connections in 1.2 s; the reconnects did not back off", n)
	}
}

// A TLS peer that stops reading (full send buffers) cannot hold Close past
// its deadline: the socket is aborted, no close_notify waits.
func TestStalledTLSPeerShutdownWithinDeadline(t *testing.T) {
	p := newPKI(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{p.server}, MinVersion: tls.VersionTLS12})
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
			_ = c.(*tls.Conn).Handshake()
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
	w := NewWriter(Options{Network: "tls", Addr: ln.Addr().String(), TLS: &tls.Config{RootCAs: p.caPool, ServerName: "localhost"}})
	big := []byte(strings.Repeat("y", 60000))
	for range 400 { // ~24 MB, far beyond the socket buffers
		w.Enqueue(big)
	}
	time.Sleep(500 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = w.Close(ctx)
	if d := time.Since(start); d > 900*time.Millisecond {
		t.Fatalf("Close took %s with a 300 ms deadline", d)
	}
}
