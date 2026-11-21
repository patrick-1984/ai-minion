package syslog

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
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

// Every failed write delays the next connection, including a failed retry.
func TestWriteFailuresBackOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const backoff = 200 * time.Millisecond
		second := []byte("second")
		dialed := make(chan time.Time, 4)
		attempt := 0
		w := NewWriter(Options{
			Network: "tcp", BackoffMin: backoff, BackoffMax: time.Second,
			Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				select {
				case dialed <- time.Now():
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				attempt++
				if attempt != 3 {
					return connWithClosedPeer(), nil
				}
				// The third link accepts one complete frame, then closes
				// so the next message detects another write failure.
				conn, peer := net.Pipe()
				go func() {
					_, _ = io.CopyN(io.Discard, peer, int64(len(OctetFrame(second))))
					_ = peer.Close()
				}()
				return conn, nil
			},
		})
		defer w.Discard()
		w.Enqueue([]byte("first"))
		w.Enqueue(second)
		w.Enqueue([]byte("third"))

		deadline := time.NewTimer(1200 * time.Millisecond)
		defer deadline.Stop()
		var times [4]time.Time
		for i := range times {
			select {
			case times[i] = <-dialed:
			case <-deadline.C:
				t.Fatalf("only %d connection attempts in 1.2 s", i)
			}
		}
		// Failed retry doubles the base, and the successful write resets it.
		for i, delay := range [...]time.Duration{backoff, 2 * backoff, backoff} {
			gap := times[i+1].Sub(times[i])
			if low, high := delay*4/5, delay*6/5; gap < low || gap > high {
				t.Fatalf("dial %d followed the previous dial after %s, want between %s and %s", i+2, gap, low, high)
			}
		}

		synctest.Wait()
		if s := w.Stats(); s.Sent != 1 || s.Dropped[DropWriteFailed] != 2 || s.Queued != 0 || len(dialed) != 0 {
			t.Fatalf("after one successful message and two failed retries: %+v", s)
		}
		// A spent retry is counted immediately, and an empty queue means
		// graceful Close has no remaining connection delay to pay.
		start := time.Now()
		if err := w.Close(context.Background()); err != nil || time.Since(start) != 0 {
			t.Fatalf("idle Close took %s: %v", time.Since(start), err)
		}
	})
}

func TestDiscardDuringBackoffAfterFailedRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		w := NewWriter(Options{
			Network: "tcp", BackoffMin: 200 * time.Millisecond, BackoffMax: time.Second,
			Dial: func(context.Context, string, string) (net.Conn, error) {
				attempts++
				return connWithClosedPeer(), nil
			},
		})
		defer w.Discard()
		w.Enqueue([]byte("first"))
		w.Enqueue([]byte("second"))
		w.Enqueue([]byte("third"))
		time.Sleep(240 * time.Millisecond) // the first retry has failed
		synctest.Wait()
		if s := w.Stats(); attempts != 2 || s.Dropped[DropWriteFailed] != 1 || s.Queued != 1 {
			t.Fatalf("waiting to dial for next message: %d attempts, stats %+v", attempts, s)
		}
		w.Discard()
		if s := w.Stats(); attempts != 2 || s.Queued != 0 || s.Sent != 0 ||
			s.Dropped[DropWriteFailed] != 1 || s.Dropped[DropReconfigure] != 2 {
			t.Fatalf("discard while waiting: %d attempts, stats %+v", attempts, s)
		}
	})
}

func TestWriteBackoffExpiresWhileIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var times []time.Time
		w := NewWriter(Options{
			Network: "tcp", BackoffMin: 200 * time.Millisecond, BackoffMax: time.Second,
			Dial: func(context.Context, string, string) (net.Conn, error) {
				times = append(times, time.Now())
				return connWithClosedPeer(), nil
			},
		})
		defer w.Discard()
		w.Enqueue([]byte("first"))
		time.Sleep(1200 * time.Millisecond)
		synctest.Wait()
		if s := w.Stats(); len(times) != 2 || s.Dropped[DropWriteFailed] != 1 {
			t.Fatalf("idle after dropping a failed retry: %d dials, stats %+v", len(times), s)
		}
		now := time.Now()
		w.Enqueue([]byte("later"))
		synctest.Wait()
		if len(times) != 3 || !times[2].Equal(now) {
			t.Fatal("the first dial after the backoff expired must be immediate")
		}
	})
}

func TestDiscardDuringRetriedWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		conn, peer := net.Pipe()
		defer func() { _ = peer.Close() }()
		w := NewWriter(Options{
			Network: "tcp", BackoffMin: 200 * time.Millisecond, BackoffMax: time.Second,
			Dial: func(context.Context, string, string) (net.Conn, error) {
				attempts++
				if attempts == 1 {
					return connWithClosedPeer(), nil
				}
				return conn, nil // the peer never reads, so the write blocks
			},
		})
		defer w.Discard()
		w.Enqueue([]byte("first"))
		time.Sleep(240 * time.Millisecond)
		synctest.Wait()
		if attempts != 2 {
			t.Fatalf("the retry should be writing on the second connection: %d dials", attempts)
		}
		w.Discard()
		if s := w.Stats(); s.Sent != 0 || s.Dropped[DropWriteFailed] != 0 || s.Dropped[DropReconfigure] != 1 {
			t.Fatalf("discard during a blocked retry: %+v", s)
		}
	})
}

func connWithClosedPeer() net.Conn {
	conn, peer := net.Pipe()
	_ = peer.Close()
	return conn
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
