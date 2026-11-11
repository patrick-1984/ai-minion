package workers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Security regression (finding "Unsolicited turns can fill the shared Hub
// database"): a Worker that answers no delivery stores nothing. Before the fix
// twelve unsolicited turns of 8 KiB were stored with the daily budget set to 1.
func TestUnsolicitedTurnsAreNotStored(t *testing.T) {
	r := newPipeRig(t, 1)
	big := strings.Repeat("x", 8192)
	stored := 0
	for i := 0; i < 12; i++ {
		id, _ := pipe.NewTurnID(r.clock.Now())
		ack, err := r.p.IngestTurn(context.Background(), wAlicePi1.ID, TurnFrame{
			Type: FrameTurn, TurnID: id, Session: pipeSess, Text: big, Time: ops.Timestamp(r.clock.Now()),
		})
		if err == nil && ack.Status == pipe.AckStored {
			stored++
		}
	}
	if stored != 0 {
		t.Fatalf("%d unsolicited turns (%d bytes) were stored", stored, stored*len(big))
	}
}

func turnOf(t *testing.T, r *pipeRig, session string, msg int64, text string) TurnFrame {
	t.Helper()
	id, err := pipe.NewTurnID(r.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return TurnFrame{Type: FrameTurn, TurnID: id, Session: session, MessageID: msg, Text: text, Time: ops.Timestamp(r.clock.Now())}
}

func ingest(t *testing.T, r *pipeRig, w WorkerRecord, f TurnFrame) pipe.Ack {
	t.Helper()
	ack, err := r.p.IngestTurn(context.Background(), w.ID, f)
	if err != nil {
		t.Fatalf("IngestTurn: %v", err)
	}
	return ack
}

func storedTurns(t *testing.T, r *pipeRig, w WorkerRecord, session string) int {
	t.Helper()
	turns, err := r.st.Turns(context.Background(), peerOfRec(w, session), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(turns)
}

// A reply is accepted once per delivery; the lost ack is answered again, and
// a second reply to the same delivery is rejected for good.
func TestAReplyAnswersItsDeliveryOnce(t *testing.T) {
	r := newPipeRig(t, 0)
	mid := r.delivery(t, wAlicePi1, pipeSess)
	first := turnOf(t, r, pipeSess, mid, "the reply")
	if ack := ingest(t, r, wAlicePi1, first); ack.Status != pipe.AckStored {
		t.Fatalf("ack = %+v", ack)
	}
	// The Worker did not see the ack: the same turn again is stored, once.
	if ack := ingest(t, r, wAlicePi1, first); ack.Status != pipe.AckStored {
		t.Fatalf("repeat: ack = %+v", ack)
	}
	// Another turn for the same delivery is final.
	second := turnOf(t, r, pipeSess, mid, "another reply")
	if ack := ingest(t, r, wAlicePi1, second); ack.Status != pipe.AckRejected || ack.Reason == "" {
		t.Fatalf("second reply: ack = %+v, want rejected", ack)
	}
	if n := storedTurns(t, r, wAlicePi1, pipeSess); n != 1 {
		t.Fatalf("%d turns stored, want 1", n)
	}
}

// The delivery must be a message of the same Worker and session, and the
// Worker is the authenticated one: a spoofed identity or message id steals
// nothing, and costs the other tenants no capacity.
func TestATurnCannotAnswerAnotherWorkersDeliveryOrSpendItsCapacity(t *testing.T) {
	r := newPipeRigWith(t, 0, func(c *PipeConfig) {
		c.Quotas = TurnQuotas{OwnerTurnBurst: 3, OwnerTurnRate: 0.001, GlobalTurnBurst: 3, GlobalTurnRate: 0.001}
	})
	bobMsg := r.delivery(t, wBobPi1, pipeSess)
	other := r.delivery(t, wAlicePi1, "minion-2")
	// Alice's Worker names Bob's delivery, and her own delivery of another
	// session: rejected, not stored, and the garbage is not charged to the
	// owner or global buckets.
	for i := 0; i < 20; i++ {
		if ack := ingest(t, r, wAlicePi1, turnOf(t, r, pipeSess, bobMsg, "spoof")); ack.Status != pipe.AckRejected {
			t.Fatalf("spoofed delivery: ack = %+v", ack)
		}
		if ack := ingest(t, r, wAlicePi1, turnOf(t, r, pipeSess, other, "spoof")); ack.Status != pipe.AckRejected {
			t.Fatalf("other session's delivery: ack = %+v", ack)
		}
		r.clock.Advance(100 * time.Millisecond) // within the frame rate
	}
	// Bob's reply still fits his owner's and the global bucket.
	if ack := ingest(t, r, wBobPi1, turnOf(t, r, pipeSess, bobMsg, "bob's reply")); ack.Status != pipe.AckStored {
		t.Fatalf("Bob's turn after Alice's spoofing: ack = %+v", ack)
	}
	if n := storedTurns(t, r, wAlicePi1, pipeSess) + storedTurns(t, r, wAlicePi1, "minion-2"); n != 0 {
		t.Fatalf("%d spoofed turns stored", n)
	}
}

func TestTurnRateIsBusyNotStoredAndRecovers(t *testing.T) {
	r := newPipeRigWith(t, 0, func(c *PipeConfig) {
		c.Quotas = TurnQuotas{WorkerTurnBurst: 3, WorkerTurnRate: 1}
	})
	var busy pipe.Ack
	stored := 0
	for i := 0; i < 10; i++ {
		ack := ingest(t, r, wAlicePi1, turnOf(t, r, pipeSess, r.delivery(t, wAlicePi1, pipeSess), "r"))
		switch ack.Status {
		case pipe.AckStored:
			stored++
		case pipe.AckBusy:
			busy = ack
		default:
			t.Fatalf("ack = %+v", ack)
		}
		// Each reply needs a new message: it is answered once.
		if _, err := r.st.AppendMessage(context.Background(), pipe.Message{Peer: peerOfRec(wAlicePi1, pipeSess), Text: "m", At: r.clock.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if stored != 3 || busy.Status != pipe.AckBusy || busy.RetryAfterMS < 1000 || busy.Reason == "" {
		t.Fatalf("stored %d, busy ack %+v: want 3 stored then busy with retry_after_ms", stored, busy)
	}
	if n := storedTurns(t, r, wAlicePi1, pipeSess); n != 3 {
		t.Fatalf("%d turns stored, a busy answer must store nothing", n)
	}
	r.clock.Advance(5 * time.Second)
	if ack := ingest(t, r, wAlicePi1, turnOf(t, r, pipeSess, r.delivery(t, wAlicePi1, pipeSess), "later")); ack.Status != pipe.AckStored {
		t.Fatalf("after the wait: ack = %+v", ack)
	}
}

func TestByteRateOfTheWorkerBoundsWhatItStoresPerSecond(t *testing.T) {
	r := newPipeRigWith(t, 0, func(c *PipeConfig) {
		c.Quotas = TurnQuotas{WorkerByteRate: 1000, WorkerByteBurst: 300 << 10}
	})
	big := strings.Repeat("x", 200<<10)
	m1, m2 := r.delivery(t, wAlicePi1, pipeSess), int64(0)
	if ack := ingest(t, r, wAlicePi1, turnOf(t, r, pipeSess, m1, big)); ack.Status != pipe.AckStored {
		t.Fatalf("first: %+v", ack)
	}
	m, _ := r.st.AppendMessage(context.Background(), pipe.Message{Peer: peerOfRec(wAlicePi1, pipeSess), Text: "m", At: r.clock.Now()})
	m2 = m.ID
	if ack := ingest(t, r, wAlicePi1, turnOf(t, r, pipeSess, m2, big)); ack.Status != pipe.AckBusy {
		t.Fatalf("second big turn at once: %+v, want busy", ack)
	}
}

// The stored volume is a quota per Worker, per owner and for the Hub, and
// retention gives it back.
func TestStoredVolumeQuotasPerWorkerOwnerAndGlobal(t *testing.T) {
	text := strings.Repeat("x", 1000)
	cost := store.TurnCost(len(text))
	cases := []struct {
		name   string
		limits store.TurnLimits
		// who is still served after the first Worker filled its share
		otherServed bool
	}{
		{"per Worker", store.TurnLimits{WorkerCost: 2 * cost}, true},
		{"per owner", store.TurnLimits{OwnerCost: 2 * cost}, true}, // another owner's Worker
		{"global", store.TurnLimits{GlobalCost: 2 * cost}, false},
		{"turn count per Worker", store.TurnLimits{WorkerTurns: 2}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPipeRigWith(t, 0, func(c *PipeConfig) { c.Quotas = TurnQuotas{Stored: tc.limits} })
			reply := func(w WorkerRecord) pipe.Ack {
				return ingest(t, r, w, turnOf(t, r, pipeSess, r.delivery(t, w, pipeSess), text))
			}
			for i := 0; i < 2; i++ {
				if ack := reply(wAlicePi1); ack.Status != pipe.AckStored {
					t.Fatalf("turn %d: %+v", i, ack)
				}
			}
			ack := reply(wAlicePi1)
			if ack.Status != pipe.AckBusy || ack.RetryAfterMS < 1000 {
				t.Fatalf("over the quota: %+v, want busy", ack)
			}
			if n := storedTurns(t, r, wAlicePi1, pipeSess); n != 2 {
				t.Fatalf("%d turns stored, want 2", n)
			}
			other := wBobPi1 // another owner
			if tc.name == "per owner" {
				// the same owner's second Worker shares the owner quota, and
				// another owner's Worker has its own.
				if ack := reply(wAlicePi2); ack.Status != pipe.AckBusy {
					t.Fatalf("same owner, other Worker: %+v, want busy", ack)
				}
			}
			got := reply(other)
			if tc.otherServed && got.Status != pipe.AckStored {
				t.Fatalf("another tenant was starved: %+v", got)
			}
			if !tc.otherServed && got.Status != pipe.AckBusy {
				t.Fatalf("global quota: %+v, want busy for everyone", got)
			}
			// Retention gives the volume back.
			r.clock.Advance(8 * 24 * time.Hour)
			if _, err := r.p.Broker().Prune(context.Background(), r.clock.Now()); err != nil {
				t.Fatal(err)
			}
			if u, err := r.st.TurnUsage(context.Background(), "worker", wAlicePi1.ID); err != nil || u.Turns != 0 || u.Cost != 0 {
				t.Fatalf("usage after prune = %+v, %v", u, err)
			}
			if ack := reply(wAlicePi1); ack.Status != pipe.AckStored {
				t.Fatalf("after retention: %+v", ack)
			}
		})
	}
}

// A repeated turn is answered stored without costing quota, even when the
// Worker is out of rate and storage.
func TestARepeatedTurnIsAckedWhileTheWorkerIsOverItsQuotas(t *testing.T) {
	r := newPipeRigWith(t, 0, func(c *PipeConfig) {
		c.Quotas = TurnQuotas{WorkerTurnBurst: 1, WorkerTurnRate: 0.0001, Stored: store.TurnLimits{WorkerTurns: 1}}
	})
	f := turnOf(t, r, pipeSess, r.delivery(t, wAlicePi1, pipeSess), "r")
	if ack := ingest(t, r, wAlicePi1, f); ack.Status != pipe.AckStored {
		t.Fatalf("first: %+v", ack)
	}
	for i := 0; i < 5; i++ {
		r.clock.Advance(50 * time.Millisecond)
		if ack := ingest(t, r, wAlicePi1, f); ack.Status != pipe.AckStored {
			t.Fatalf("repeat %d: %+v, want the lost ack again", i, ack)
		}
	}
}

func TestDatabasePressureRefusesTurnsAndRecovers(t *testing.T) {
	t.Run("database", func(t *testing.T) {
		r := newPipeRigWith(t, 0, func(c *PipeConfig) { c.Quotas = TurnQuotas{MaxDBBytes: 1} })
		ack := ingest(t, r, wAlicePi1, turnOf(t, r, pipeSess, r.delivery(t, wAlicePi1, pipeSess), "r"))
		if ack.Status != pipe.AckBusy {
			t.Fatalf("under pressure: %+v, want busy", ack)
		}
		if n := storedTurns(t, r, wAlicePi1, pipeSess); n != 0 {
			t.Fatalf("%d turns stored under pressure", n)
		}
	})
	// The log that a reader keeps from being written back is real pressure: the
	// gate closes while the reader is open and opens when it ends.
	t.Run("wal", func(t *testing.T) {
		r := newPipeRigWith(t, 0, func(c *PipeConfig) { c.Quotas = TurnQuotas{MaxWALBytes: 1} })
		ctx := context.Background()
		conn, err := r.db.DB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, `BEGIN DEFERRED`); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pipe_messages`).Scan(&n); err != nil { // the reader takes its snapshot
			t.Fatal(err)
		}
		delivery := r.delivery(t, wAlicePi1, pipeSess) // writes behind the reader's snapshot
		if pr, err := r.st.Pressure(ctx, 1); err != nil || pr.WALBytes == 0 {
			t.Fatalf("pressure = %+v, %v: the open reader must keep the log outstanding", pr, err)
		}
		ack := ingest(t, r, wAlicePi1, turnOf(t, r, pipeSess, delivery, "r"))
		if ack.Status != pipe.AckBusy {
			t.Fatalf("under pressure: %+v, want busy", ack)
		}
		if got := storedTurns(t, r, wAlicePi1, pipeSess); got != 0 {
			t.Fatalf("%d turns stored under pressure", got)
		}
		if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
			t.Fatal(err)
		}
		r.clock.Advance(2 * pressureTTL)
		if ack := ingest(t, r, wAlicePi1, turnOf(t, r, pipeSess, delivery, "r")); ack.Status != pipe.AckStored {
			t.Fatalf("after the reader ended: %+v, want stored", ack)
		}
	})
}

// The Hub gives the Worker the id of the stored message, whatever the caller
// put in the arguments.
func TestSendRoutesTheStoredMessageIdAndIgnoresTheCallers(t *testing.T) {
	r := newPipeRig(t, 0)
	args := sendArgs(wAlicePi1, pipeSess, "hello")
	args.MessageID = 987654 // a caller cannot choose it
	if _, err := r.p.Send(context.Background(), authz.NewSessionPrincipal(idAlice), args); err != nil {
		t.Fatal(err)
	}
	msgs, _ := r.st.Messages(context.Background(), peerOfRec(wAlicePi1, pipeSess), 0, 0)
	got, ok := r.rt.last().Args.(ops.PipeSendArgs)
	if len(msgs) != 1 || !ok || got.MessageID != msgs[0].ID {
		t.Fatalf("routed message_id = %+v for stored %+v", got, msgs)
	}
	// The reply names it, and is accepted.
	if ack := ingest(t, r, wAlicePi1, turnOf(t, r, pipeSess, got.MessageID, "the reply")); ack.Status != pipe.AckStored {
		t.Fatalf("reply: %+v", ack)
	}
}

// A Worker that ignores busy answers loses its stream, and its connection
// when it does so again and again; capacity is never taken for the frames.
func TestAWorkerThatIgnoresBusyLosesItsStreamAndConnection(t *testing.T) {
	var aborted atomic.Int32
	r := newPipeRigWith(t, 0, func(c *PipeConfig) {
		c.Quotas = TurnQuotas{WorkerFrameBurst: 2, WorkerFrameRate: 0.0001}
		c.Abort = func(id string) {
			if id == wAlicePi1.ID {
				aborted.Add(1)
			}
		}
	})
	for round := 0; round < abuseStrikes; round++ {
		var items []readItem
		for i := 0; i < 3*maxBusyStrikes; i++ {
			items = append(items, turnJSON(t, "x", 0, pipeSess, "flood", r.clock.Now()))
		}
		fs := newFakeStream(items...)
		r.p.ServeTurns(context.Background(), wAlicePi1.ID, deadlineStream{fs})
		fs.mu.Lock()
		answered := len(fs.written)
		fs.mu.Unlock()
		reset := fs.wasReset()
		if answered > maxBusyStrikes+3 || !reset {
			t.Fatalf("round %d: %d answers, reset %v: the stream was served on", round, answered, reset)
		}
	}
	if aborted.Load() != 1 {
		t.Fatalf("connection aborted %d times, want once after %d resets", aborted.Load(), abuseStrikes)
	}
}

// roundOfTurns is one stream of a Worker that sends n replies, each to a new
// delivery of its session, and the stream the Hub served.
func roundOfTurns(t *testing.T, r *pipeRig, n int) (*fakeStream, []TurnAckFrame) {
	t.Helper()
	var items []readItem
	for i := 0; i < n; i++ {
		m, err := r.st.AppendMessage(context.Background(), pipe.Message{Peer: peerOfRec(wAlicePi1, pipeSess), Text: "m", At: r.clock.Now()})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, turnJSON(t, turnIDAt(t, r), m.ID, pipeSess, "reply", r.clock.Now()))
	}
	fs := newFakeStream(items...)
	r.p.ServeTurns(context.Background(), wAlicePi1.ID, deadlineStream{fs})
	return fs, writtenAcks(t, fs)
}

func turnIDAt(t *testing.T, r *pipeRig) string {
	t.Helper()
	id, err := pipe.NewTurnID(r.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A Worker that obeys the holds is never cut off, however long the period in
// which the Hub is out of capacity lasts: here a full stored-volume quota for 100
// minutes. Every round is a window of 8 turns on a new stream (a reconnect or a
// reopened channel), the first of which finds the hold over and is answered
// busy again; the other 7 were in flight. That is 800 busy answers, far more
// than 32 in a row and far more than 3 resets, with no reset and no abort.
func TestACompliantWorkerWaitingOutAStoragePeriodIsNeverCutOff(t *testing.T) {
	var aborted atomic.Int32
	r := newPipeRigWith(t, 0, func(c *PipeConfig) {
		c.Quotas = TurnQuotas{Stored: store.TurnLimits{WorkerTurns: 1}}
		c.Abort = func(string) { aborted.Add(1) }
	})
	busyTotal := 0
	for round := 0; round < 100; round++ {
		fs, acks := roundOfTurns(t, r, 8)
		if fs.wasReset() {
			t.Fatalf("round %d: the Hub reset the stream of a Worker that waited for its hold", round)
		}
		for _, a := range acks {
			if a.Status == pipe.AckBusy {
				busyTotal++
				if a.RetryAfterMS < 1000 {
					t.Fatalf("round %d: retry_after_ms = %d", round, a.RetryAfterMS)
				}
			}
		}
		r.clock.Advance(61 * time.Second) // the hold is a minute: a compliant retry
	}
	if busyTotal < 4*maxBusyStrikes {
		t.Fatalf("only %d busy answers: the test did not reach the old strike bound", busyTotal)
	}
	if aborted.Load() != 0 {
		t.Fatalf("the connection of a compliant Worker was closed %d times", aborted.Load())
	}
	if n := storedTurns(t, r, wAlicePi1, pipeSess); n != 1 {
		t.Fatalf("%d turns stored, want the one the quota allows", n)
	}
}

// A first attempt is never early, however long the Hub told the Worker to wait
// before: a Worker that waits out a hold of about a quarter of an hour, several
// times in a row, keeps everything. The hold also outlives the stream: the frame
// of a reopened channel before the end of it is early, and counted.
func TestALongRetryAfterSurvivesStreamsAndCountsEarlyFrames(t *testing.T) {
	var aborted atomic.Int32
	r := newPipeRigWith(t, 0, func(c *PipeConfig) {
		c.Quotas = TurnQuotas{WorkerTurnRate: 0.001, WorkerTurnBurst: 1}
		c.Abort = func(string) { aborted.Add(1) }
	})
	early := func() int {
		r.p.limiter.mu.Lock()
		defer r.p.limiter.mu.Unlock()
		if h := r.p.limiter.holds[wAlicePi1.ID]; h != nil {
			return h.early
		}
		return 0
	}
	_, acks := roundOfTurns(t, r, 2)
	if len(acks) != 2 || acks[0].Status != pipe.AckStored || acks[1].Status != pipe.AckBusy || acks[1].RetryAfterMS < 900_000 {
		t.Fatalf("acks = %+v, want stored then busy with a long retry_after", acks)
	}
	wait := time.Duration(acks[1].RetryAfterMS) * time.Millisecond

	// Reopened channels before the end of the hold: early, and counted across streams.
	for i := 1; i <= 5; i++ {
		_, acks := roundOfTurns(t, r, 1)
		if len(acks) != 1 || acks[0].Status != pipe.AckBusy {
			t.Fatalf("early frame %d: %+v", i, acks)
		}
		if got := early(); got != i {
			t.Fatalf("early frames counted = %d, want %d (the count belongs to the Worker, not to the stream)", got, i)
		}
		r.clock.Advance(2 * time.Second)
	}
	// The end of the hold: a compliant retry starts the count again and is taken.
	r.clock.Advance(wait)
	_, acks = roundOfTurns(t, r, 1)
	if len(acks) != 1 || acks[0].Status != pipe.AckStored {
		t.Fatalf("after the hold: %+v", acks)
	}
	if early() != 0 {
		t.Fatalf("early frames = %d after a compliant retry", early())
	}
	if aborted.Load() != 0 {
		t.Fatal("the connection was closed")
	}
}

// Early repeats across streams are abuse and are cut: each reset needs another
// 32 early frames (the count starts again on a new stream), and the third reset
// closes the connection. Garbage that is not a turn costs nothing but its read;
// a turn that is rejected for good during a hold is not a busy answer and does
// not count.
func TestEarlyRepeatsAcrossStreamsAreStruckAndMalformedFramesAreNot(t *testing.T) {
	var aborted atomic.Int32
	r := newPipeRigWith(t, 0, func(c *PipeConfig) {
		c.Quotas = TurnQuotas{Stored: store.TurnLimits{WorkerTurns: 1}}
		c.Abort = func(string) { aborted.Add(1) }
	})
	roundOfTurns(t, r, 2) // one stored, one busy: a hold of a minute is on

	// Malformed and unsolicited frames during the hold: no busy answer, no strike.
	// (Frames of type turn are charged to the frame rate of the Worker whatever
	// they hold, burst 100: a flood beyond it is busy, early, and struck like any
	// other early frame, which TestAWorkerThatIgnoresBusyLosesItsStreamAndConnection
	// shows. Here the Worker stays inside the burst.)
	var junk []readItem
	for i := 0; i < 20; i++ {
		junk = append(junk, readItem{body: json.RawMessage(`{"type":"turn","turn_id":"x"}`)}, readItem{body: json.RawMessage(`not json`)}, readItem{body: json.RawMessage(`{"type":"noise"}`)})
	}
	fs := newFakeStream(junk...)
	r.p.ServeTurns(context.Background(), wAlicePi1.ID, deadlineStream{fs})
	if fs.wasReset() || aborted.Load() != 0 {
		t.Fatalf("malformed frames during a hold were struck (reset %v, aborts %d)", fs.wasReset(), aborted.Load())
	}

	resets := 0
	for round := 0; round < 60 && aborted.Load() == 0; round++ {
		fs, _ := roundOfTurns(t, r, 8)
		if fs.wasReset() {
			resets++
		}
		r.clock.Advance(time.Second) // far less than the hold: every frame is early
	}
	if aborted.Load() != 1 || resets != abuseStrikes {
		t.Fatalf("aborts %d, resets %d: want %d resets and then the connection closed once", aborted.Load(), resets, abuseStrikes)
	}
}

// frameTokens returns the frame tokens left of a Worker, or -1 for a Worker
// whose bucket was never created.
func frameTokens(r *pipeRig, id string) float64 {
	r.p.limiter.mu.Lock()
	defer r.p.limiter.mu.Unlock()
	if m := r.p.limiter.frames[id]; m != nil {
		return m.turns.tokens
	}
	return -1
}

// Every inbound frame of the turn channel is charged before anything is decided
// about it: a frame of the wrong shape, a frame of an unknown type and a frame
// that is no JSON at all cost what a turn costs, and a run of them ends the
// stream. The Worker that sends 500 of them with a burst of one gets no free
// token for the well-shaped turn that follows.
func TestEveryInboundFrameIsChargedBeforeItIsJudged(t *testing.T) {
	shapes := map[string]string{
		"wrong shape":  `{"type":"turn","turn_id":{}}`,
		"unknown type": `{"type":"surprise","x":1}`,
		"not a frame":  `{"type":`,
	}
	for name, body := range shapes {
		t.Run(name, func(t *testing.T) {
			var aborted atomic.Int32
			r := newPipeRigWith(t, 0, func(c *PipeConfig) {
				c.Quotas = TurnQuotas{WorkerFrameBurst: 1, WorkerFrameRate: 0.0001}
				c.Abort = func(string) { aborted.Add(1) }
			})
			var items []readItem
			for i := 0; i < 500; i++ {
				items = append(items, readItem{body: json.RawMessage(body)})
			}
			items = append(items, turnJSON(t, turnIDAt(t, r), 1, pipeSess, "reply", r.clock.Now()))
			fs := newFakeStream(items...)
			r.p.ServeTurns(context.Background(), wAlicePi1.ID, deadlineStream{fs})
			if !fs.wasReset() {
				t.Fatalf("500 %s frames did not end the stream", name)
			}
			if got := frameTokens(r, wAlicePi1.ID); got < 0 || got >= 1 {
				t.Fatalf("frame tokens left = %v: the frames were not charged", got)
			}
			fs.mu.Lock()
			read := len(items) - len(fs.reads)
			fs.mu.Unlock()
			if read > maxJunkFrames+1 {
				t.Fatalf("%d frames were read before the stream ended; a flood must be cut at %d", read, maxJunkFrames)
			}
			// The well-shaped turn that follows does not get a free token: the
			// bucket is empty and the next turn is busy.
			ack, err := r.p.IngestTurn(context.Background(), wAlicePi1.ID, turnOf(t, r, pipeSess, 1, "x"))
			if err != nil || ack.Status != pipe.AckBusy {
				t.Fatalf("the turn after the flood: %+v, %v, want busy", ack, err)
			}
		})
	}
}

// A single unknown frame is forward compatibility (AD-4), and a turn between
// them resets the run.
func TestAnUnknownFrameBetweenTurnsIsStillIgnored(t *testing.T) {
	r := newPipeRig(t, 0)
	var items []readItem
	for i := 0; i < 45; i++ { // 45 unknown frames and 15 turns: inside the frame burst
		items = append(items, readItem{body: json.RawMessage(`{"type":"future.frame"}`)})
		m, err := r.st.AppendMessage(context.Background(), pipe.Message{Peer: peerOfRec(wAlicePi1, pipeSess), Text: "m", At: r.clock.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			items = append(items, turnJSON(t, turnIDAt(t, r), m.ID, pipeSess, "reply", r.clock.Now()))
		}
	}
	fs := newFakeStream(items...)
	r.p.ServeTurns(context.Background(), wAlicePi1.ID, deadlineStream{fs})
	if fs.wasReset() {
		t.Fatal("unknown frames between turns ended the stream")
	}
}

// The bytes of the frames are a budget of their own: a few large frames of
// junk exhaust it, whatever they hold, and the Worker is told to wait.
func TestTheBytesOfJunkFramesAreCharged(t *testing.T) {
	r := newPipeRigWith(t, 0, func(c *PipeConfig) {
		c.Quotas = TurnQuotas{WorkerFrameByteBurst: 1, WorkerFrameByteRate: 1}
	})
	big := json.RawMessage(`{"type":"surprise","pad":"` + strings.Repeat("x", 600<<10) + `"}`)
	fs := newFakeStream(readItem{body: big}, readItem{body: big}, readItem{body: big})
	r.p.ServeTurns(context.Background(), wAlicePi1.ID, deadlineStream{fs})
	if _, ok := r.p.limiter.frame(wAlicePi1.ID, 600<<10); ok {
		t.Fatal("the bytes of three 600 KiB frames were not charged: another one fits")
	}
	r.p.limiter.mu.Lock()
	bytesLeft := r.p.limiter.frames[wAlicePi1.ID].bytes.tokens
	r.p.limiter.mu.Unlock()
	if bytesLeft > 600<<10 {
		t.Fatalf("%v byte tokens left of a 1 MiB budget after 600 KiB frames", bytesLeft)
	}
}

// TestWrongShapeTurnsOverRealYamuxAreChargedAndCutOff is the audit's real-path
// reproduction: a Worker on a real yamux session, through the host and the
// ingest, writes frames of the wrong shape with a frame burst of one. They are
// charged (the bucket is spent before the well-shaped turn arrives), and the
// stream is ended after a bounded run, which the Worker sees as the end of its
// stream.
func TestWrongShapeTurnsOverRealYamuxAreChargedAndCutOff(t *testing.T) {
	w := newWorld(t, wAlicePi1)
	w.p.limiter = newTurnLimiter(TurnQuotas{WorkerFrameBurst: 1, WorkerFrameRate: 0.0001}, w.clock.Now)
	st, err := w.workers[wAlicePi1.ID].sess.OpenEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.WriteFrame(map[string]string{"type": FrameTurnsOpen}); err != nil {
		t.Fatal(err)
	}
	written := 0
	for i := 0; i < 500; i++ {
		if err := st.WriteFrame(json.RawMessage(`{"type":"turn","turn_id":{}}`)); err != nil {
			break
		}
		written++
	}
	_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := st.ReadFrame(); err == nil {
		t.Fatal("the Hub answered a flood of wrong-shape frames instead of ending the stream")
	}
	w.p.limiter.mu.Lock()
	m := w.p.limiter.frames[wAlicePi1.ID]
	left := -1.0
	if m != nil {
		left = m.turns.tokens
	}
	w.p.limiter.mu.Unlock()
	if left < 0 || left >= 1 {
		t.Fatalf("frame tokens left = %v after %d wrong-shape frames: they were not charged", left, written)
	}
}

// TestPressureRecoversAfterRetentionAndCheckpoint is the audit's reproduction
// through the production gate, with scaled thresholds: a burst of data crosses the
// gate, the retention deletes it and a checkpoint writes the log back, and after
// the pressure reading's TTL the gate admits turns again, although the database
// file still holds 256 free pages and the WAL file is as large as it was.
func TestPressureRecoversAfterRetentionAndCheckpoint(t *testing.T) {
	for _, scope := range []string{"database", "wal"} {
		t.Run(scope, func(t *testing.T) {
			r := newPipeRig(t, 0)
			ctx := context.Background()
			db := r.db.DB()
			if _, err := db.Exec(`CREATE TABLE pressure_probe (payload BLOB)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
				t.Fatal(err)
			}
			baseline, err := r.st.Pressure(ctx, 1<<40)
			if err != nil {
				t.Fatal(err)
			}
			if scope == "database" {
				r.p.quotas.MaxDBBytes = baseline.DBBytes + 512<<10
			} else {
				r.p.quotas.MaxWALBytes = 512 << 10
			}
			// A reader pins the log while the burst is written, so that the WAL
			// scope has outstanding frames (the database scope does not need it).
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.ExecContext(ctx, `BEGIN DEFERRED`); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pressure_probe`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO pressure_probe VALUES (zeroblob(1048576))`); err != nil {
				t.Fatal(err)
			}
			if !r.p.underPressure(ctx) {
				t.Fatal("the burst did not reach the gate")
			}
			if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`DELETE FROM pressure_probe`); err != nil {
				t.Fatal(err)
			}
			var busy, frames, done int
			if err := db.QueryRow(`PRAGMA wal_checkpoint(FULL)`).Scan(&busy, &frames, &done); err != nil || busy != 0 || frames != done {
				t.Fatalf("checkpoint: busy %d, frames %d, done %d, %v", busy, frames, done, err)
			}
			var free int
			if err := db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil || free < 200 {
				t.Fatalf("free pages = %d, %v: the file must still hold the freed pages", free, err)
			}
			r.clock.Advance(2 * pressureTTL)
			if r.p.underPressure(ctx) {
				pr, _ := r.st.Pressure(ctx, r.p.quotas.MaxWALBytes)
				t.Fatalf("the gate stayed closed after retention and checkpoint: %+v (free pages %d)", pr, free)
			}
			// And a turn goes through it.
			if ack := ingest(t, r, wAlicePi1, turnOf(t, r, pipeSess, r.delivery(t, wAlicePi1, pipeSess), "r")); ack.Status != pipe.AckStored {
				t.Fatalf("a turn after the recovery: %+v", ack)
			}
		})
	}
}

// Round 2 (T2): a receiver that definitively refuses a message with
// pipe_origin_refused delivered nothing, so refused sends must not use up the
// recipient's hourly quota; a legitimate send still goes through afterwards.
// Other errors may hide a delivery and keep their charge.
func TestRefusedSendsDoNotConsumeTheHourlyQuota(t *testing.T) {
	ctx := context.Background()
	send := func(r *pipeRig, text string) error {
		_, err := r.p.Send(ctx, authz.NewSessionPrincipal(idAlice),
			ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target(wAlicePi1, pipeSess), Text: text})
		return err
	}
	t.Run("pipe_origin_refused is refunded", func(t *testing.T) {
		r := newPipeRig(t, 100000)
		r.rt.err = proto.NewError(proto.CodePipeOriginRefused, "refused")
		for i := 0; i < pipe.SendsPerHour+20; i++ {
			if err := send(r, "attempt"); err == nil {
				t.Fatal("a refused send succeeded")
			} else if strings.Contains(err.Error(), "limit") {
				t.Fatalf("attempt %d hit the rate limit: %v", i, err)
			}
			r.clock.Advance(2 * time.Second) // attempt throttling still applies per second
		}
		r.rt.err = nil
		if err := send(r, "legitimate"); err != nil {
			t.Fatalf("a legitimate send after %d refusals: %v", pipe.SendsPerHour+20, err)
		}
	})
	t.Run("attempt throttling stays", func(t *testing.T) {
		r := newPipeRig(t, 100000)
		r.rt.err = proto.NewError(proto.CodePipeOriginRefused, "refused")
		throttled := false
		for i := 0; i < pipe.SendBurst+3; i++ { // no clock advance: faster than 1/s
			if err := send(r, "burst"); err != nil && strings.Contains(err.Error(), "fast") {
				throttled = true
			}
		}
		if !throttled {
			t.Fatal("refunded sends bypassed the per-second attempt throttle")
		}
	})
	t.Run("an unknown outcome keeps its charge", func(t *testing.T) {
		r := newPipeRig(t, 100000)
		r.rt.err = proto.NewError(proto.CodeInternal, "lost")
		for i := 0; i < pipe.SendsPerHour; i++ {
			_ = send(r, "attempt")
			r.clock.Advance(2 * time.Second)
		}
		r.rt.err = nil
		err := send(r, "legitimate")
		if err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("charges for sends of unknown outcome were refunded: %v", err)
		}
	})
}

// failingDeleteStore fails every DeleteMessage.
type failingDeleteStore struct{ PipeStore }

func (failingDeleteStore) DeleteMessage(context.Context, int64) error { return errors.New("disk full") }

// A refused send whose stored message cannot be deleted is not refunded and
// the same request_id is never routed again.
func TestRefusedSendWithFailedDeleteIsNotRefundedNorRerouted(t *testing.T) {
	ctx := context.Background()
	r := newPipeRigWith(t, 100000, func(c *PipeConfig) { c.Store = failingDeleteStore{c.Store} })
	r.rt.err = proto.NewError(proto.CodePipeOriginRefused, "refused")
	args := ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target(wAlicePi1, pipeSess), Text: "x"}
	pr := authz.NewSessionPrincipal(idAlice)
	if _, err := r.p.Send(ctx, pr, args); err == nil {
		t.Fatal("a refused send succeeded")
	}
	if n := r.rt.count(); n != 1 {
		t.Fatalf("routed %d times, want 1", n)
	}
	// The retry with the same request_id answers with the recorded refusal.
	r.rt.err = nil
	wr, err := r.p.Send(ctx, pr, args)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if wr.Outcome == ops.OutcomeDelivered || r.rt.count() != 1 {
		t.Fatalf("the retained request was routed again: %+v, calls=%d", wr, r.rt.count())
	}
	// No refund: the hourly quota stays charged by the refused send.
	ok := 0
	for i := 0; i < pipe.SendsPerHour+5; i++ {
		r.clock.Advance(2 * time.Second)
		_, err = r.p.Send(ctx, pr, ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target(wAlicePi1, pipeSess), Text: "y"})
		if err == nil {
			ok++
		}
	}
	if ok != pipe.SendsPerHour-1 {
		t.Fatalf("%d sends fit after the refused one, want %d (the charge was refunded)", ok, pipe.SendsPerHour-1)
	}
}
