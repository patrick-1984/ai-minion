package workers

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/transport"
)

// lossyStream is a stream whose first `lose` read interrupts are lost, the way a
// yamux wake-up is lost when another reader of the stream takes it.
type lossyStream struct {
	lose  int32
	calls atomic.Int32
	woke  chan struct{}
	once  sync.Once
}

func (l *lossyStream) SetReadDeadline(time.Time) error {
	if l.calls.Add(1) > l.lose {
		l.once.Do(func() { close(l.woke) })
	}
	return nil
}

// A lost wake-up must not leave the handler asleep: the interrupt is repeated
// until the handler stops it, and it stops with the handler.
func TestInterruptOnDoneRepeatsUntilTheReaderIsWoken(t *testing.T) {
	l := &lossyStream{lose: 3, woke: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	stop := interruptOnDone(ctx, l)
	cancel()
	select {
	case <-l.woke:
	case <-time.After(5 * time.Second):
		t.Fatalf("the reader was not woken after %d interrupts", l.calls.Load())
	}
	stop() // returns only when the interrupt goroutine has ended
	n := l.calls.Load()
	time.Sleep(4 * interruptEvery)
	if got := l.calls.Load(); got != n {
		t.Errorf("interrupts went on after stop: %d -> %d", n, got)
	}
}

func TestInterruptOnDoneStoppedBeforeTheEndDoesNothing(t *testing.T) {
	l := &lossyStream{woke: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	stop := interruptOnDone(ctx, l)
	stop()
	cancel()
	time.Sleep(4 * interruptEvery)
	if n := l.calls.Load(); n != 0 {
		t.Errorf("a stopped interrupt fired %d times", n)
	}
}

// The shape of the failure of TestServerCancellationInterruptsIdleEventTurnAndCall
// StreamsBeforeWaiting, on a private session: a handler waits in a read of a real
// stream, and the end of its context both interrupts the read and resets the
// stream (serveFeed does the latter), which starts a drain that reads the same
// stream at the same time. Which of the two readers gets the single yamux wake-up
// is a matter of timing, so the end is repeated; every round must end within
// the bound. The bound is a barrier for a hang, not a timing assertion: a round
// takes milliseconds.
func TestIdleReadEndsWithItsContextWhileTheStreamIsDrained(t *testing.T) {
	hubSide, workerSide := net.Pipe()
	hub, err := transport.NewSession(hubSide, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := transport.NewSession(workerSide, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close(); _ = worker.Close() })

	rounds := 300
	if testing.Short() {
		rounds = 40
	}
	for i := 0; i < rounds; i++ {
		octx, ocancel := context.WithTimeout(context.Background(), 5*time.Second)
		ws, err := worker.OpenEvents(octx)
		if err != nil {
			ocancel()
			t.Fatalf("round %d: open: %v", i, err)
		}
		hs, err := hub.Accept(octx)
		ocancel()
		if err != nil {
			t.Fatalf("round %d: accept: %v", i, err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		read := make(chan error, 1)
		reading := make(chan struct{})
		go func() {
			defer interruptOnDone(ctx, hs)()
			close(reading)
			_, err := hs.ReadFrame()
			read <- err
		}()
		<-reading
		time.Sleep(time.Duration(i%4) * time.Millisecond) // vary the instant the read starts to wait
		stopReset := context.AfterFunc(ctx, hs.Reset)
		cancel()
		select {
		case err := <-read:
			if err == nil {
				t.Fatalf("round %d: the read returned a frame", i)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: the idle read did not end with its context (a wake-up was lost)", i)
		}
		stopReset()
		hs.Reset()
		ws.Reset()
	}
}
