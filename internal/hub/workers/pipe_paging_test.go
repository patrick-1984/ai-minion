package workers

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// ingest stores a received turn without moving the clock, so that several
// entries can share one millisecond. It does not go through the ingestion
// checks (see commit); ingestWire does.
func (r *pipeRig) ingest(t *testing.T, w WorkerRecord, session, text string, _ time.Time) string {
	t.Helper()
	id, err := pipe.NewTurnID(r.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.PutTurn(context.Background(), pipe.Turn{TurnID: id, Peer: peerOfRec(w, session), Text: text, At: r.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	return id
}

// ingestWire is ingest through the Hub's ingestion, answering a delivery; the
// time of the Worker is in the frame.
func (r *pipeRig) ingestWire(t *testing.T, w WorkerRecord, session, text string, workerTime time.Time) string {
	t.Helper()
	id, err := pipe.NewTurnID(r.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	ack, err := r.p.IngestTurn(context.Background(), w.ID, TurnFrame{
		Type: FrameTurn, TurnID: id, Session: session, MessageID: r.delivery(t, w, session), Text: text, Time: ops.Timestamp(workerTime),
	})
	if err != nil || ack.Status != pipe.AckStored {
		t.Fatalf("IngestTurn = %+v, %v", ack, err)
	}
	return id
}

func (r *pipeRig) send(t *testing.T, text string) {
	t.Helper()
	if _, err := r.p.Send(context.Background(), authz.NewSessionPrincipal(idAlice), sendArgs(wAlicePi1, pipeSess, text)); err != nil {
		t.Fatalf("Send(%q): %v", text, err)
	}
}

func viewTexts(v PipeReadView) []string {
	out := make([]string, 0, len(v.Turns))
	for _, e := range v.Turns {
		out = append(out, string(e.Direction)+":"+e.Text)
	}
	return out
}

// --- the Hub clock stamps committed turns ---

func TestTurnsAreStampedWithTheHubClockNotTheWorkers(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	alice := authz.NewSessionPrincipal(idAlice)
	r.send(t, "question")
	r.clock.Advance(2 * time.Second)
	// A Worker without an RTC before NTP sync: its clock is 30 days behind.
	r.ingestWire(t, wAlicePi1, pipeSess, "answer", r.clock.Now().Add(-30*24*time.Hour))

	view, err := r.p.Read(ctx, alice, ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess)})
	if err != nil {
		t.Fatal(err)
	}
	if got := viewTexts(view); len(got) != 2 || got[0] != "sent:question" || got[1] != "received:answer" {
		t.Fatalf("pipe_read = %v, want the answer after the question", got)
	}
	if at := view.Turns[1].At.Time(); !at.Equal(r.clock.Now()) {
		t.Fatalf("turn time = %v, want the Hub's now %v", at, r.clock.Now())
	}

	// The retention counts from the Hub time: the hourly prune keeps it.
	if res, err := r.p.Broker().Prune(ctx, r.clock.Now().Add(time.Hour)); err != nil || res.Turns != 0 || res.Messages != 0 {
		t.Fatalf("Prune = %+v, %v", res, err)
	}
	view, _ = r.p.Read(ctx, alice, ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess)})
	if len(view.Turns) != 2 {
		t.Fatalf("after the prune: %v", viewTexts(view))
	}
	// ... and removes it once the Hub's 7 days are over.
	if res, _ := r.p.Broker().Prune(ctx, r.clock.Now().Add(8*24*time.Hour)); res.Turns != 1 || res.Messages != 1 {
		t.Fatalf("Prune after 8 days = %+v", res)
	}
}

func TestATurnWhoseEncodingCannotFitAFrameIsRejected(t *testing.T) {
	r := newPipeRig(t, 0)
	id, _ := pipe.NewTurnID(r.clock.Now())
	// 256 KiB of '<' is 1.5 MiB once encoded: it could never be read back in one frame.
	ack, err := r.p.IngestTurn(context.Background(), wAlicePi1.ID, TurnFrame{
		Type: FrameTurn, TurnID: id, Session: pipeSess, MessageID: r.delivery(t, wAlicePi1, pipeSess), Text: strings.Repeat("<", MaxTurnBytes), Time: ops.Timestamp(r.clock.Now()),
	})
	if err != nil || ack.Status != pipe.AckRejected {
		t.Fatalf("IngestTurn = %+v, %v, want rejected", ack, err)
	}
}

// --- paging: every turn_id that pipe_read returns is a cursor ---

// pageAll reads the pipe one entry at a time and returns the turn_ids and
// texts in order.
func pageAll(t *testing.T, r *pipeRig, pr authz.Principal) (ids, got []string, withheld int) {
	t.Helper()
	after := ""
	for i := 0; i < 100; i++ {
		v, err := r.p.Read(context.Background(), pr, ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess), AfterTurnID: after, Limit: 1})
		if err != nil {
			t.Fatalf("page %d after %q: %v", i, after, err)
		}
		withheld += v.Withheld
		if len(v.Turns) == 0 {
			return ids, got, withheld
		}
		if len(v.Turns) != 1 {
			t.Fatalf("limit 1 returned %d entries", len(v.Turns))
		}
		e := v.Turns[0]
		ids = append(ids, e.TurnID)
		got = append(got, string(e.Direction)+":"+e.Text)
		after = e.TurnID
	}
	t.Fatal("paging does not end")
	return nil, nil, 0
}

func TestPagingAMixedHistoryOneEntryAtATimeSeesEveryEntryOnce(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	// Several entries share a millisecond: m1 m2 a b, then later m3 c d.
	r.send(t, "m1")
	r.send(t, "m2")
	r.ingest(t, wAlicePi1, pipeSess, "a", time.Time{})
	r.ingest(t, wAlicePi1, pipeSess, "b", time.Time{})
	r.clock.Advance(time.Second)
	r.send(t, "m3")
	r.ingest(t, wAlicePi1, pipeSess, "c", time.Time{})
	r.ingest(t, wAlicePi1, pipeSess, "d", time.Time{})
	want := []string{"sent:m1", "sent:m2", "received:a", "received:b", "sent:m3", "received:c", "received:d"}

	full, err := r.p.Read(ctx, authz.NewSessionPrincipal(idAlice), ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess), Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got := viewTexts(full); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("full read = %v, want %v", got, want)
	}

	for name, pr := range map[string]authz.Principal{
		"a person":       authz.NewSessionPrincipal(idAlice),
		"a trusting key": modelAlice(),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "a trusting key" {
				if err := r.trust.Set(ctx, idAlice, wAlicePi1.ID, pipeSess); err != nil {
					t.Fatal(err)
				}
			}
			ids, got, withheld := pageAll(t, r, pr)
			if strings.Join(got, ",") != strings.Join(want, ",") || withheld != 0 {
				t.Fatalf("paged = %v (withheld %d), want %v", got, withheld, want)
			}
			seen := map[string]bool{}
			for _, id := range ids {
				if seen[id] || !pipe.IsTurnID(id) {
					t.Fatalf("turn_id %q repeated or not a UUIDv7 in %v", id, ids)
				}
				seen[id] = true
			}
		})
	}
}

func TestPagingOnlySentMessagesAndWithheldHistories(t *testing.T) {
	r := newPipeRig(t, 0)
	// Only sent messages: they can be paged.
	r.send(t, "s1")
	r.send(t, "s2")
	r.clock.Advance(time.Millisecond)
	r.send(t, "s3")
	_, got, _ := pageAll(t, r, authz.NewSessionPrincipal(idAlice))
	if strings.Join(got, ",") != "sent:s1,sent:s2,sent:s3" {
		t.Fatalf("only sent: %v", got)
	}

	// A model that does not trust the source: the page holds only sent
	// entries, which still page; every received turn is counted as withheld
	// once.
	r2 := newPipeRig(t, 0)
	r2.send(t, "m1")
	r2.ingest(t, wAlicePi1, pipeSess, "a", time.Time{})
	r2.send(t, "m2")
	r2.ingest(t, wAlicePi1, pipeSess, "b", time.Time{})
	r2.clock.Advance(time.Second)
	r2.ingest(t, wAlicePi1, pipeSess, "c", time.Time{})
	_, got, withheld := pageAll(t, r2, modelAlice())
	if strings.Join(got, ",") != "sent:m1,sent:m2" || withheld != 3 {
		t.Fatalf("untrusted model: %v withheld %d, want [sent:m1 sent:m2] and 3", got, withheld)
	}
	v, _ := r2.p.Read(context.Background(), modelAlice(), ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess)})
	if v.Reason != ReasonUntrustedPeer || v.Withheld != 3 || len(v.Turns) != 2 {
		t.Fatalf("one big page: %+v", v)
	}
}

func TestACursorOfAnotherPeerOrAnUnknownEntryIsNotFound(t *testing.T) {
	r := newPipeRig(t, 0)
	r.send(t, "m1")
	other := pipe.Peer{WorkerID: wAlicePi1.ID, SessionName: "minion-2"}
	r.ingest(t, wAlicePi1, "minion-2", "elsewhere", time.Time{})
	v, _ := r.p.Read(context.Background(), authz.NewSessionPrincipal(idAlice), ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess)})
	sentOfThis := v.Turns[0].TurnID
	// The synthetic id of a message of minion-1 is not a cursor of minion-2.
	_, err := r.p.Read(context.Background(), authz.NewSessionPrincipal(idAlice), ops.PipeReadArgs{Target: ops.Target{WorkerID: other.WorkerID, SessionName: other.SessionName}, AfterTurnID: sentOfThis})
	wantCode(t, err, proto.CodeNotFound)
	// A made-up UUIDv7 is not found either, and a non-UUID is invalid.
	_, err = r.p.Read(context.Background(), authz.NewSessionPrincipal(idAlice), ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess), AfterTurnID: ops.NewUUIDv7()})
	wantCode(t, err, proto.CodeNotFound)
	_, err = r.p.Read(context.Background(), authz.NewSessionPrincipal(idAlice), ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess), AfterTurnID: "nope"})
	wantCode(t, err, proto.CodeInvalidArgument)
}

func TestReadSendWaitFlowAcceptsTheLastEntryAsCursor(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	alice := authz.NewSessionPrincipal(idAlice)
	r.ingest(t, wAlicePi1, pipeSess, "earlier reply", time.Time{})
	r.clock.Advance(time.Millisecond)
	r.send(t, "first question")
	v, err := r.p.Read(ctx, alice, ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess)})
	if err != nil || len(v.Turns) != 2 || v.Turns[1].Direction != ops.PipeSent {
		t.Fatalf("read = %+v, %v", v, err)
	}
	cursor := v.Turns[len(v.Turns)-1].TurnID // a sent entry
	r.clock.Advance(time.Millisecond)
	r.send(t, "second question")

	got := make(chan []ops.PipeTurn, 1)
	errc := make(chan error, 1)
	go func() {
		var turns []ops.PipeTurn
		err := r.p.Wait(ctx, alice, ops.PipeWaitArgs{Target: target(wAlicePi1, pipeSess), AfterTurnID: cursor, TimeoutMS: 5000},
			func(e ops.PipeTurn) error { turns = append(turns, e); return nil })
		got <- turns
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	r.clock.Advance(time.Millisecond)
	r.ingest(t, wAlicePi1, pipeSess, "the answer", time.Time{})
	select {
	case turns := <-got:
		if err := <-errc; err != nil || len(turns) != 1 || turns[0].Text != "the answer" {
			t.Fatalf("Wait = %+v, %v", turns, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pipe_wait did not return")
	}
	// A turn committed after the sent message but before the wait started is delivered at once.
	var late []ops.PipeTurn
	if err := r.p.Wait(ctx, alice, ops.PipeWaitArgs{Target: target(wAlicePi1, pipeSess), AfterTurnID: cursor, TimeoutMS: 50},
		func(e ops.PipeTurn) error { late = append(late, e); return nil }); err != nil || len(late) != 1 {
		t.Fatalf("Wait after a stored turn = %+v, %v", late, err)
	}
}

// --- a retried pipe_send is the same send ---

func TestRetryOfASendIsNeitherStoredNorChargedAgain(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 1) // one message per day
	alice := authz.NewSessionPrincipal(idAlice)
	args := sendArgs(wAlicePi1, pipeSess, "once")
	if _, err := r.p.Send(ctx, alice, args); err != nil {
		t.Fatal(err)
	}
	// The answer was lost; the caller asks again with the same request_id.
	res, err := r.p.Send(ctx, alice, args)
	if err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("retry = %+v, %v, want the original outcome", res, err)
	}
	msgs, _ := r.st.Messages(ctx, peerOfRec(wAlicePi1, pipeSess), 0, 0)
	if len(msgs) != 1 {
		t.Fatalf("%d messages stored, want 1", len(msgs))
	}
	if r.rt.count() != 1 {
		t.Fatalf("%d calls routed: the retry is answered from the recorded outcome and is not routed again", r.rt.count())
	}
	// A new request is a new send and the budget of 1 is spent.
	_, err = r.p.Send(ctx, alice, sendArgs(wAlicePi1, pipeSess, "twice"))
	wantCode(t, err, proto.CodePipeBudgetExceeded)

	// A retry while the Worker is offline still gets the recorded answer and
	// retracts nothing the first attempt delivered.
	r.rt.mu.Lock()
	r.rt.err = proto.NewError(proto.CodeWorkerOffline, "offline")
	r.rt.mu.Unlock()
	if res, err := r.p.Send(ctx, alice, args); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("retry while offline = %+v, %v, want the recorded outcome", res, err)
	}
	if msgs, _ := r.st.Messages(ctx, peerOfRec(wAlicePi1, pipeSess), 0, 0); len(msgs) != 1 {
		t.Fatalf("%d messages after a retry, want the first one kept", len(msgs))
	}
}

func TestConcurrentRetriesStoreOneMessage(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	alice := authz.NewSessionPrincipal(idAlice)
	args := sendArgs(wAlicePi1, pipeSess, "once")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.p.Send(ctx, alice, args); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if msgs, _ := r.st.Messages(ctx, peerOfRec(wAlicePi1, pipeSess), 0, 0); len(msgs) != 1 {
		t.Fatalf("%d messages stored, want 1", len(msgs))
	}
}

func TestRetryAfterAnUndeliveredSendStoresAgain(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	alice := authz.NewSessionPrincipal(idAlice)
	args := sendArgs(wAlicePi1, pipeSess, "busy agent")
	r.rt.mu.Lock()
	r.rt.result = ops.Refused("busy", nil)
	r.rt.mu.Unlock()
	if _, err := r.p.Send(ctx, alice, args); err != nil {
		t.Fatal(err)
	}
	r.rt.mu.Lock()
	r.rt.result = deliveredResult()
	r.rt.mu.Unlock()
	r.clock.Advance(time.Second)
	if _, err := r.p.Send(ctx, alice, args); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := r.st.Messages(ctx, peerOfRec(wAlicePi1, pipeSess), 0, 0); len(msgs) != 1 {
		t.Fatalf("%d messages stored, want the delivered one", len(msgs))
	}
}

// --- frame sizes over real call streams ---

// readOverCall runs pipe_read on a call stream of the Worker and returns the
// answer; it fails the test on a reset.
func (ww *worldWorker) readOverCall(t *testing.T, args map[string]any) (proto.Code, PipeReadView) {
	t.Helper()
	code, res := ww.call(t, ops.OpPipeRead, args, "")
	var v PipeReadView
	if code == "" {
		if err := json.Unmarshal(res, &v); err != nil {
			t.Fatalf("result: %v", err)
		}
	}
	return code, v
}

func TestPipeReadOverACallStreamIsCutByEncodedSizeAndPagesOn(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, wAlicePi1, wAlicePi2)
	if err := w.trust.Set(ctx, idAlice, wAlicePi1.ID, "minion-1"); err != nil {
		t.Fatal(err)
	}
	// 50 long agent replies of 25 KiB: 1.25 MiB, more than one frame.
	var want []string
	for i := 0; i < 50; i++ {
		text := strings.Repeat(string(rune('a'+i%26)), 25<<10)
		w.ingest(t, wAlicePi1, "minion-1", text, time.Time{})
		w.clock.Advance(time.Millisecond)
		want = append(want, text)
	}
	ww := w.workers[wAlicePi1.ID] // a Worker identity reads its own Worker's sessions
	after := ""
	var got []string
	pages := 0
	for len(got) < len(want) && pages < 50 {
		args := map[string]any{"target": "pi-1:minion-1"}
		if after != "" {
			args["after_turn_id"] = after
		}
		code, v := ww.readOverCall(t, args)
		if code != "" {
			t.Fatalf("page %d: error %q instead of a cut page", pages, code)
		}
		if len(v.Turns) == 0 {
			t.Fatalf("page %d is empty after %d entries", pages, len(got))
		}
		for _, e := range v.Turns {
			got = append(got, e.Text)
		}
		after = v.Turns[len(v.Turns)-1].TurnID
		pages++
	}
	if pages < 2 {
		t.Fatalf("read in %d page(s): the 1.25 MiB history must not fit one frame", pages)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d differs", i)
		}
	}
}

func TestOversizedAnswerOnACallStreamIsAnErrorFrameNotAReset(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, wAlicePi1, wAlicePi2)
	if err := w.trust.Set(ctx, idAlice, wAlicePi1.ID, "minion-1"); err != nil {
		t.Fatal(err)
	}
	peer := peerOfRec(wAlicePi1, "minion-1")
	first, _ := pipe.NewTurnID(w.clock.Now())
	// A turn that is stored although it cannot be sent in one frame (an
	// older Hub's row): 256 KiB of '<' is 1.5 MiB encoded.
	big, _ := pipe.NewTurnID(w.clock.Now().Add(time.Millisecond))
	for i, id := range []string{first, big} {
		text := "small"
		if i == 1 {
			text = strings.Repeat("<", MaxTurnBytes)
		}
		if _, err := w.st.PutTurn(ctx, pipe.Turn{TurnID: id, Peer: peer, Text: text, At: w.clock.Now().Add(time.Duration(i) * time.Millisecond)}); err != nil {
			t.Fatal(err)
		}
	}
	ww := w.workers[wAlicePi1.ID] // a Worker identity reads its own Worker's sessions
	// pipe_read of the big turn alone: an error frame, the call is answered.
	code, _ := ww.call(t, ops.OpPipeRead, map[string]any{"target": "pi-1:minion-1", "after_turn_id": first}, "")
	if code != proto.CodeFrameTooLarge {
		t.Fatalf("pipe_read of an oversized turn: code %q, want frame_too_large", code)
	}
	// pipe_wait delivers it as a progress frame: also an error frame.
	code, _ = ww.call(t, ops.OpPipeWait, map[string]any{"target": "pi-1:minion-1", "after_turn_id": first, "timeout_ms": 1000}, "")
	if code != proto.CodeFrameTooLarge {
		t.Fatalf("pipe_wait of an oversized turn: code %q, want frame_too_large", code)
	}
	// The connection is intact: the next call works.
	code, v := ww.readOverCall(t, map[string]any{"target": "pi-1:minion-1", "limit": 1})
	if code != "" || len(v.Turns) != 1 || v.Turns[0].Text != "small" {
		t.Fatalf("a call after it: %q %+v", code, v)
	}
}

// --- live paging: an entry committed after the cursor is never skipped ---

// The reader holds a turn as its cursor. A message that the Hub commits in the
// same millisecond afterwards used to sort before the turn (messages ranked
// before turns of a millisecond) and was skipped for ever.
func TestLivePagingKeepsAMessageCommittedInTheSameMillisecondAfterTheCursorTurn(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	alice := authz.NewSessionPrincipal(idAlice)
	turn := r.ingest(t, wAlicePi1, pipeSess, "the reply", time.Time{})
	page, err := r.p.Read(ctx, alice, ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess), Limit: 1})
	if err != nil || len(page.Turns) != 1 || page.Turns[0].TurnID != turn {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	r.send(t, "sent after the reply, same millisecond") // the fake clock has not moved
	next, err := r.p.Read(ctx, alice, ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess), AfterTurnID: turn})
	if err != nil || len(next.Turns) != 1 || next.Turns[0].Direction != ops.PipeSent || next.Turns[0].Text != "sent after the reply, same millisecond" {
		t.Fatalf("page after the cursor = %+v, %v: the message was lost", next, err)
	}
}

// The other direction, and a clock that stepped back: the order is the commit
// order, the cursor is the last entry the reader got.
func TestLivePagingFollowsTheCommitOrderWhateverTheClockSays(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	alice := authz.NewSessionPrincipal(idAlice)
	read := func(after string) PipeReadView {
		t.Helper()
		v, err := r.p.Read(ctx, alice, ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess), AfterTurnID: after, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	r.send(t, "m1")
	v := read("")
	if got := viewTexts(v); len(got) != 1 || got[0] != "sent:m1" {
		t.Fatalf("page 1 = %v", got)
	}
	cursor := v.Turns[0].TurnID // the synthetic id of a sent message
	r.ingest(t, wAlicePi1, pipeSess, "t1", time.Time{})
	r.clock.Advance(-time.Hour) // the Hub's clock steps back
	r.send(t, "m2")
	var seen []string
	for i := 0; i < 5; i++ {
		v = read(cursor)
		if len(v.Turns) == 0 {
			break
		}
		seen = append(seen, viewTexts(v)...)
		cursor = v.Turns[len(v.Turns)-1].TurnID
	}
	if len(seen) != 2 || seen[0] != "received:t1" || seen[1] != "sent:m2" {
		t.Fatalf("entries after m1 = %v, want t1 then m2 in commit order", seen)
	}
}
