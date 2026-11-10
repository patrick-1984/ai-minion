package workers

import (
	"context"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Security regression (finding "Authenticated Workers can retain unbounded
// live event streams"): a Worker that keeps opening idle turn channels, and
// idle call streams, on one live connection must not hold more than a small,
// fixed number of streams on the Hub, however long the connection stays up.

// idleStreamBound is the most idle stream goroutines the Hub may keep for one
// Worker in these tests; the limits of one Worker are far below it.
const idleStreamBound = 24

func openIdleTurnChannels(t *testing.T, ww *worldWorker, n int) []*transport.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out []*transport.Stream
	for i := 0; i < n; i++ {
		st, err := ww.sess.OpenEvents(ctx)
		if err != nil {
			break // the Hub ended the session: that is a valid answer to abuse
		}
		out = append(out, st)
		if err := st.WriteFrame(map[string]string{"type": FrameTurnsOpen}); err != nil {
			break
		}
	}
	return out
}

func TestIdleTurnChannelsOfOneWorkerAreBounded(t *testing.T) {
	w := newWorld(t, wAlicePi1)
	ww := w.workers[wAlicePi1.ID]
	opened := openIdleTurnChannels(t, ww, 400)
	if len(opened) < 40 {
		t.Fatalf("the probe opened only %d turn channels", len(opened))
	}
	time.Sleep(500 * time.Millisecond)
	checkYamuxBound(t, ww)
	if n := streamGoroutines(); n > idleStreamBound {
		t.Fatalf("%d idle turn channels of one Worker keep %d stream goroutines on the Hub (bound %d)", len(opened), n, idleStreamBound)
	}
}

func TestIdleCallStreamsOfOneWorkerAreBounded(t *testing.T) {
	w := newWorld(t, wAlicePi1)
	ww := w.workers[wAlicePi1.ID]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opened := 0
	for i := 0; i < 400; i++ {
		if _, err := ww.sess.OpenCall(ctx); err != nil {
			break
		}
		opened++
	}
	if opened < 40 {
		t.Fatalf("the probe opened only %d call streams", opened)
	}
	time.Sleep(500 * time.Millisecond)
	checkYamuxBound(t, ww)
	if n := streamGoroutines(); n > idleStreamBound {
		t.Fatalf("%d idle call streams of one Worker keep %d stream goroutines on the Hub (bound %d)", opened, n, idleStreamBound)
	}
}

// yamuxStreamBound is the most inbound streams the Hub's session may hold open
// while it is still running.
const yamuxStreamBound = 48

// checkYamuxBound: a session that is still up holds a bounded number of
// streams (admitted, being refused or closing); an abusive one was ended.
func checkYamuxBound(t *testing.T, ww *worldWorker) {
	t.Helper()
	if ww.hub.Err() != nil {
		return
	}
	if n := ww.hub.NumStreams(); n > yamuxStreamBound {
		t.Fatalf("the Hub's session of one Worker holds %d streams (bound %d) and is still up", n, yamuxStreamBound)
	}
}

func waitUsage(t *testing.T, a *streamAdmission, what string, ok func(admissionUsage) bool) admissionUsage {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		u := a.usage()
		if ok(u) {
			return u
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: usage = %+v", what, u)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Idle call streams use only the call lane: the event stream and the turn
// channel of the same Worker keep working while the call lane is full.
func TestIdleCallsDoNotStarveTheEventAndTurnChannels(t *testing.T) {
	w := newWorld(t, wAlicePi1)
	ww := w.workers[wAlicePi1.ID]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < DefaultWorkerCalls; i++ {
		if _, err := ww.sess.OpenCall(ctx); err != nil {
			t.Fatal(err)
		}
	}
	waitUsage(t, w.handler.streamAdmission(), "the call lane to fill", func(u admissionUsage) bool { return u.Calls == DefaultWorkerCalls })
	// The 9th call is refused, the connection stays.
	if code, _ := ww.call(t, "send_text", map[string]any{}, ""); code == "" {
		t.Fatal("the call over the limit was not answered with an error")
	}
	if ack := ww.emit(t, "minion-1", "reply", w.clock.Now()); ack.Status != pipe.AckStored {
		t.Fatalf("the turn channel was starved by idle calls: ack = %+v", ack)
	}
	ev, err := ww.sess.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.WriteFrame(snapFrame(0, map[string]any{"name": "minion-1", "kind": "agent:claude", "state": "stable", "task": "t"})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the event stream to be served beside the idle calls", func() bool {
		s, ok := w.events.Sessions(wAlicePi1.ID)
		return ok && len(s.Sessions) == 1
	})
}

// A second turn channel replaces the first one: the Hub interrupts the old
// reader (it does not just forget it), and only one turn channel is served.
func TestANewTurnChannelEndsTheReaderOfTheOldOne(t *testing.T) {
	w := newWorld(t, wAlicePi1)
	ww := w.workers[wAlicePi1.ID]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var old *transport.Stream
	for i := 0; i < 3; i++ {
		st, err := ww.sess.OpenEvents(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.WriteFrame(map[string]string{"type": FrameTurnsOpen}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			old = st
		}
		time.Sleep(50 * time.Millisecond)
	}
	readFails(t, "replaced turn channel", old, 3*time.Second)
	eventually(t, "one turn channel to be served", func() bool { return goroutinesIn("(*Pipe).ServeTurns") == 1 })
}

// A slot is held until the stream is really released: a Worker that does not
// complete the close of its stream keeps the slot, so closing and reopening
// streams does not give it more than its share.
func TestASlotIsHeldUntilTheStreamIsReleased(t *testing.T) {
	w := newWorld(t, wAlicePi1)
	ww := w.workers[wAlicePi1.ID]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a := w.handler.streamAdmission()
	var streams []*transport.Stream
	for i := 0; i < DefaultWorkerEvents; i++ {
		st, err := ww.sess.OpenEvents(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// An unknown first frame: the Hub's handler resets the stream at once.
		if err := st.WriteFrame(map[string]string{"type": "nothing"}); err != nil {
			t.Fatal(err)
		}
		streams = append(streams, st)
	}
	time.Sleep(300 * time.Millisecond)
	if u := a.usage(); u.Events != DefaultWorkerEvents {
		t.Fatalf("the Hub gave the slots back while the Worker had not closed its streams: %+v", u)
	}
	for _, st := range streams {
		_ = st.Close() // the Worker completes the close
	}
	waitUsage(t, a, "the slots to be released with the streams", func(u admissionUsage) bool { return u.Events == 0 })
}

// The limits of the whole Hub hold across Workers, and the counters of a
// session that ends are given back.
func TestGlobalStreamLimitAndCleanup(t *testing.T) {
	w := newWorld(t, wAlicePi1, wAlicePi2, wBobPi1)
	a := w.handler.streamAdmission()
	a.lim.GlobalEvents = 5
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, rec := range []WorkerRecord{wAlicePi1, wAlicePi2, wBobPi1} {
		ww := w.workers[rec.ID]
		for i := 0; i < 3; i++ {
			st, err := ww.sess.OpenEvents(ctx)
			if err != nil {
				break
			}
			_ = st.WriteFrame(map[string]string{"type": FrameTurnsOpen})
		}
	}
	u := waitUsage(t, a, "the global ev lane to fill", func(u admissionUsage) bool { return u.Events == 5 })
	time.Sleep(200 * time.Millisecond)
	if u2 := a.usage(); u2.Events > 5 || u.Events > 5 {
		t.Fatalf("global ev streams = %+v", u2)
	}
	w.cancel() // the Hub stops: every session ends
	waitUsage(t, a, "cleanup", func(u admissionUsage) bool {
		return u.Events == 0 && u.Calls == 0 && u.Workers == 0 && u.Writers == 0
	})
}

// An abusive connection is ended, and the refusals it caused are bounded: no
// goroutine per refused stream.
func TestAbusiveConnectionIsEndedAndRefusalsAreBounded(t *testing.T) {
	w := newWorld(t, wAlicePi1)
	ww := w.workers[wAlicePi1.ID]
	a := w.handler.streamAdmission()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	before := runtimeGoroutines()
	for i := 0; i < 600; i++ {
		if _, err := ww.sess.OpenCall(ctx); err != nil {
			break
		}
	}
	select {
	case <-ww.hub.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("the Hub kept a connection that opened 600 calls with %d held", ww.hub.NumStreams())
	}
	waitUsage(t, a, "capacity to be given back", func(u admissionUsage) bool { return u.Calls == 0 && u.Writers == 0 })
	if grown := runtimeGoroutines() - before; grown > 40 {
		t.Fatalf("%d goroutines grew from refused streams", grown)
	}
}

func runtimeGoroutines() int { return runtime.NumGoroutine() }

// goroutinesIn counts the goroutines whose stack has marker.
func goroutinesIn(marker string) int {
	buf := make([]byte, 1<<22)
	n := runtime.Stack(buf, true)
	count := 0
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.Contains(g, marker) {
			count++
		}
	}
	return count
}
