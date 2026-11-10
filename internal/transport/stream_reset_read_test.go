package transport

import (
	"context"
	"errors"
	"net"
	"runtime"
	"testing"
	"time"
)

// nativePair returns a private Hub and Worker session over net.Pipe, and one
// stream pair of kind ev: the Worker side opens it, the Hub side accepts it.
func nativePair(t *testing.T) (hub, worker *Session) {
	t.Helper()
	hubSide, workerSide := net.Pipe()
	hub, err := NewSession(hubSide, RoleHub, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err = NewSession(workerSide, RoleWorker, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close(); _ = worker.Close() })
	return hub, worker
}

func openPair(t *testing.T, hub, worker *Session) (hs, ws *Stream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	ws, err := worker.OpenEvents(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	hs, err = hub.Accept(ctx)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	return hs, ws
}

// A read that waits on a stream must end when the stream is reset, by Reset
// alone: the drain that Reset starts must not take the wake-up of the read nor
// move its read deadline. Repeated, because who gets yamux's single
// notification is a matter of timing (this failed within a few rounds of the
// drain and the read sharing the stream).
func TestResetEndsTheReadThatWaitsWhileTheStreamIsDrained(t *testing.T) {
	hub, worker := nativePair(t)
	rounds := 300
	if testing.Short() {
		rounds = 40
	}
	for i := 0; i < rounds; i++ {
		hs, ws := openPair(t, hub, worker)
		read := make(chan error, 1)
		reading := make(chan struct{})
		go func() {
			close(reading)
			_, err := hs.ReadFrame()
			read <- err
		}()
		<-reading
		runtime.Gosched()
		hs.Reset()
		select {
		case err := <-read:
			if err == nil {
				t.Fatalf("round %d: the read returned a frame", i)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: the waiting read did not end with the reset", i)
		}
		ws.Reset()
	}
}

// After Reset a read fails at once and never waits behind the drain, which
// goes on reading the stream until the peer has closed it.
func TestReadAfterResetFailsWithoutWaitingBehindTheDrain(t *testing.T) {
	hub, worker := nativePair(t)
	hs, ws := openPair(t, hub, worker)
	hs.Reset()
	done := make(chan error, 1)
	go func() {
		_, err := hs.Read(make([]byte, 8))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrStreamReset) {
			t.Fatalf("read after reset: %v, want ErrStreamReset", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a read after Reset waited behind the drain")
	}
	if _, err := hs.ReadFrame(); !errors.Is(err, ErrStreamReset) {
		t.Fatalf("ReadFrame after reset: %v", err)
	}
	// The peer still sending is drained, and Release returns when the peer
	// has completed the close: the reservation is held until then.
	_ = ws.WriteFrame(map[string]any{"type": "x"})
	released := make(chan struct{})
	go func() { hs.Release(); close(released) }()
	select {
	case <-released:
		t.Fatal("Release returned before the peer closed the stream")
	case <-time.After(100 * time.Millisecond):
	}
	_ = ws.Close()
	select {
	case <-released:
	case <-time.After(ReleaseBound + testWait):
		t.Fatal("Release did not return after the peer closed")
	}
	ws.Release()
}

// Release called while a read waits ends that read and still waits for the
// real release.
func TestReleaseEndsTheReadThatWaits(t *testing.T) {
	hub, worker := nativePair(t)
	hs, ws := openPair(t, hub, worker)
	read := make(chan error, 1)
	go func() { _, err := hs.ReadFrame(); read <- err }()
	time.Sleep(20 * time.Millisecond) // let the read reach its wait; the bound below does not depend on it
	released := make(chan struct{})
	go func() { hs.Release(); close(released) }()
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("read returned a frame")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the waiting read did not end with Release")
	}
	_ = ws.Close()
	select {
	case <-released:
	case <-time.After(ReleaseBound + testWait):
		t.Fatal("Release did not return")
	}
	ws.Release()
}

// A protocol violation still resets the stream and reports the protocol error,
// and the stream is drained afterwards (the existing behavior, with reads
// going through the single-reader rule).
func TestProtocolViolationStillResetsAndReportsIt(t *testing.T) {
	hub, worker := nativePair(t)
	hs, ws := openPair(t, hub, worker)
	if _, err := ws.Write(lenPrefix(1 << 30)); err != nil {
		t.Fatal(err)
	}
	if _, err := hs.ReadFrame(); err == nil || errors.Is(err, ErrStreamReset) {
		t.Fatalf("oversized frame: %v, want the protocol error", err)
	}
	if _, err := hs.ReadFrame(); !errors.Is(err, ErrStreamReset) {
		t.Fatalf("after the violation: %v, want ErrStreamReset", err)
	}
	_ = ws.Close()
	hs.Release()
	ws.Release()
}
