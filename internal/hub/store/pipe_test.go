package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/pipe"
)

var (
	pipeT0   = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	pipePeer = pipe.Peer{WorkerID: "w-1", SessionName: "minion-1"}
	pipeOth  = pipe.Peer{WorkerID: "w-1", SessionName: "minion-2"}
)

func turnID(t *testing.T, at time.Time) string {
	t.Helper()
	id, err := pipe.NewTurnID(at)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPipeStoreMessages(t *testing.T) {
	ctx := context.Background()
	ps := openTest(t).Pipe()
	var ids []int64
	for i, txt := range []string{"a", "b", "c"} {
		m, err := ps.AppendMessage(ctx, pipe.Message{Peer: pipePeer, Text: txt, At: pipeT0.Add(time.Duration(i) * time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		if m.ID == 0 || (len(ids) > 0 && m.ID <= ids[len(ids)-1]) {
			t.Fatalf("id %d does not grow after %v", m.ID, ids)
		}
		ids = append(ids, m.ID)
	}
	if _, err := ps.AppendMessage(ctx, pipe.Message{Peer: pipeOth, Text: "other", At: pipeT0}); err != nil {
		t.Fatal(err)
	}

	got, err := ps.Messages(ctx, pipePeer, 0, 0)
	if err != nil || len(got) != 3 || got[0].Text != "a" || got[2].Text != "c" || got[0].Peer != pipePeer || !got[1].At.Equal(pipeT0.Add(time.Second)) {
		t.Fatalf("Messages = %+v, %v", got, err)
	}
	if got, _ = ps.Messages(ctx, pipePeer, ids[0], 1); len(got) != 1 || got[0].Text != "b" {
		t.Fatalf("after the first, limit 1 = %+v", got)
	}
	n, err := ps.CountMessagesSince(ctx, pipePeer, pipeT0.Add(time.Second))
	if err != nil || n != 2 {
		t.Fatalf("CountMessagesSince = %d, %v; want 2 (at or after)", n, err)
	}
	if err := ps.DeleteMessage(ctx, ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := ps.DeleteMessage(ctx, 9999); err != nil {
		t.Fatalf("deleting a missing message: %v", err)
	}
	if n, _ = ps.CountMessagesSince(ctx, pipePeer, pipeT0); n != 2 {
		t.Fatalf("count after delete = %d", n)
	}
}

func TestPipeStoreTurnsAreIdempotentAndPaged(t *testing.T) {
	ctx := context.Background()
	ps := openTest(t).Pipe()
	var ids []string
	for i := 0; i < 3; i++ {
		at := pipeT0.Add(time.Duration(i) * time.Second)
		id := turnID(t, at)
		ids = append(ids, id)
		ins, err := ps.PutTurn(ctx, pipe.Turn{TurnID: id, Peer: pipePeer, Text: "reply", At: at})
		if err != nil || !ins {
			t.Fatalf("PutTurn %d = %v, %v", i, ins, err)
		}
	}
	// The same turn_id again changes nothing, whatever the new content.
	ins, err := ps.PutTurn(ctx, pipe.Turn{TurnID: ids[0], Peer: pipePeer, Text: "changed", At: pipeT0.Add(time.Hour)})
	if err != nil || ins {
		t.Fatalf("repeated PutTurn = %v, %v; want not inserted", ins, err)
	}
	all, err := ps.Turns(ctx, pipePeer, "", 0)
	if err != nil || len(all) != 3 || all[0].TurnID != ids[0] || all[0].Text != "reply" || all[2].TurnID != ids[2] {
		t.Fatalf("Turns = %+v, %v", all, err)
	}
	after, err := ps.Turns(ctx, pipePeer, ids[0], 1)
	if err != nil || len(after) != 1 || after[0].TurnID != ids[1] {
		t.Fatalf("Turns after the first, limit 1 = %+v, %v", after, err)
	}
	if got, _ := ps.Turns(ctx, pipeOth, "", 0); len(got) != 0 {
		t.Fatalf("turns of another session = %+v", got)
	}
	if _, err := ps.Turns(ctx, pipeOth, ids[0], 0); !errors.Is(err, pipe.ErrNotFound) {
		t.Fatalf("cursor of another peer = %v, want not_found", err)
	}
	if _, err := ps.Turns(ctx, pipePeer, turnID(t, pipeT0), 0); !errors.Is(err, pipe.ErrNotFound) {
		t.Fatalf("unknown cursor = %v, want not_found", err)
	}
}

func TestPipeStoreDeleteBefore(t *testing.T) {
	ctx := context.Background()
	ps := openTest(t).Pipe()
	for i := 0; i < 4; i++ {
		at := pipeT0.Add(time.Duration(i) * time.Hour)
		if _, err := ps.AppendMessage(ctx, pipe.Message{Peer: pipePeer, Text: "m", At: at}); err != nil {
			t.Fatal(err)
		}
		if _, err := ps.PutTurn(ctx, pipe.Turn{TurnID: turnID(t, at), Peer: pipePeer, Text: "t", At: at}); err != nil {
			t.Fatal(err)
		}
	}
	msgs, turns, err := ps.DeleteBefore(ctx, pipeT0.Add(2*time.Hour))
	if err != nil || msgs != 2 || turns != 2 {
		t.Fatalf("DeleteBefore = %d, %d, %v; want 2, 2", msgs, turns, err)
	}
	if left, _ := ps.Messages(ctx, pipePeer, 0, 0); len(left) != 2 {
		t.Fatalf("%d messages left", len(left))
	}
	if left, _ := ps.Turns(ctx, pipePeer, "", 0); len(left) != 2 {
		t.Fatalf("%d turns left", len(left))
	}
}

// The broker over the SQLite store: the limits and the idempotent commit are
// the broker's, the store only keeps what it is given.
func TestBrokerOverTheHubStore(t *testing.T) {
	ctx := context.Background()
	clock := pipeT0
	b, err := pipe.NewBroker(pipe.Config{Store: openTest(t).Pipe(), Now: func() time.Time { return clock }, BudgetPerDay: 7})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < pipe.SendBurst; i++ {
		if _, err := b.Send(ctx, pipePeer, "hi"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if _, err := b.Send(ctx, pipePeer, "hi"); !errors.Is(err, pipe.ErrPipeRateLimited) {
		t.Fatalf("sixth send = %v, want pipe_rate_limited", err)
	}
	clock = clock.Add(10 * time.Second)
	for i := 0; i < 2; i++ {
		if _, err := b.Send(ctx, pipePeer, "hi"); err != nil {
			t.Fatalf("send after the wait: %v", err)
		}
	}
	clock = clock.Add(10 * time.Second)
	if _, err := b.Send(ctx, pipePeer, "hi"); !errors.Is(err, pipe.ErrPipeBudgetExceeded) {
		t.Fatalf("eighth send = %v, want pipe_budget_exceeded", err)
	}

	id := turnID(t, clock)
	for i := 0; i < 2; i++ {
		ack, err := b.CommitTurn(ctx, pipe.Turn{TurnID: id, Peer: pipePeer, Text: "reply"})
		if err != nil || ack.Status != pipe.AckStored {
			t.Fatalf("commit %d = %+v, %v", i, ack, err)
		}
	}
	if got, _ := b.Turns(ctx, pipePeer, "", 0); len(got) != 1 {
		t.Fatalf("%d turns stored after two commits", len(got))
	}
	res, err := b.Prune(ctx, clock.Add(pipe.Retention+time.Hour))
	if err != nil || res.Messages != 7 || res.Turns != 1 {
		t.Fatalf("Prune = %+v, %v", res, err)
	}
}

func TestPipeStorePositionsOrderMessagesAndTurnsOnOneClock(t *testing.T) {
	ctx := context.Background()
	ps := openTest(t).Pipe()
	at := func(ms int) time.Time { return pipeT0.Add(time.Duration(ms) * time.Millisecond) }
	m1, _ := ps.AppendMessage(ctx, pipe.Message{Peer: pipePeer, Text: "m1", At: at(0)})
	m2, _ := ps.AppendMessage(ctx, pipe.Message{Peer: pipePeer, Text: "m2", At: at(0)})
	t1, t2 := turnID(t, at(0)), turnID(t, at(0))
	for _, id := range []string{t1, t2} {
		if _, err := ps.PutTurn(ctx, pipe.Turn{TurnID: id, Peer: pipePeer, Text: "t", At: at(0)}); err != nil {
			t.Fatal(err)
		}
	}
	m3, _ := ps.AppendMessage(ctx, pipe.Message{Peer: pipePeer, Text: "m3", At: at(5)})

	// The entries are in the order they were committed, whatever the times say.
	p1, ok, err := ps.MessagePos(ctx, pipePeer, m1.ID, at(0).UnixMilli())
	if err != nil || !ok {
		t.Fatalf("MessagePos = %+v, %v, %v", p1, ok, err)
	}
	tp1, err := ps.TurnPos(ctx, pipePeer, t1)
	if err != nil {
		t.Fatal(err)
	}
	if !p1.Less(tp1) || tp1.Less(p1) {
		t.Fatalf("positions %+v and %+v are not in commit order", p1, tp1)
	}
	msgs, err := ps.MessagesAfterPos(ctx, pipePeer, p1, 0)
	if err != nil || len(msgs) != 2 || msgs[0].ID != m2.ID || msgs[1].ID != m3.ID {
		t.Fatalf("MessagesAfterPos = %+v, %v", msgs, err)
	}
	turns, err := ps.TurnsAfterPos(ctx, pipePeer, p1, 0)
	if err != nil || len(turns) != 2 || turns[0].TurnID != t1 || turns[1].TurnID != t2 {
		t.Fatalf("TurnsAfterPos(message) = %+v, %v", turns, err)
	}
	if turns, _ = ps.TurnsAfterPos(ctx, pipePeer, tp1, 0); len(turns) != 1 || turns[0].TurnID != t2 {
		t.Fatalf("TurnsAfterPos(turn) = %+v", turns)
	}
	if msgs, _ = ps.MessagesAfterPos(ctx, pipePeer, tp1, 0); len(msgs) != 1 || msgs[0].ID != m3.ID {
		t.Fatalf("MessagesAfterPos(turn) = %+v", msgs)
	}
	if all, _ := ps.TurnsAfterPos(ctx, pipePeer, PipePos{}, 1); len(all) != 1 || all[0].TurnID != t1 {
		t.Fatalf("TurnsAfterPos(zero, 1) = %+v", all)
	}
	// Counting turns between two positions.
	p3, _, _ := ps.MessagePos(ctx, pipePeer, m3.ID, at(5).UnixMilli())
	if n, err := ps.CountTurnsAfterPos(ctx, pipePeer, PipePos{}, nil); err != nil || n != 2 {
		t.Fatalf("CountTurnsAfterPos = %d, %v", n, err)
	}
	if n, _ := ps.CountTurnsAfterPos(ctx, pipePeer, p1, &p1); n != 0 {
		t.Fatalf("an empty range counted %d", n)
	}
	if n, _ := ps.CountTurnsAfterPos(ctx, pipePeer, p1, &p3); n != 2 {
		t.Fatalf("(m1, m3] counted %d turns", n)
	}
	if n, _ := ps.CountTurnsAfterPos(ctx, pipePeer, tp1, &p3); n != 1 {
		t.Fatalf("(t1, m3] counted %d turns", n)
	}
	if last, err := ps.LatestTurnPos(ctx, pipePeer); err != nil || last.Commit == 0 || last.Less(tp1) {
		t.Fatalf("LatestTurnPos = %+v, %v", last, err)
	}
	if last, err := ps.LatestTurnPos(ctx, pipeOth); err != nil || last != (PipePos{}) {
		t.Fatalf("LatestTurnPos of an empty peer = %+v, %v", last, err)
	}
	// A message position needs the right peer and the right time.
	if _, ok, _ := ps.MessagePos(ctx, pipeOth, m1.ID, at(0).UnixMilli()); ok {
		t.Fatal("a message of another peer has a position")
	}
	if _, ok, _ := ps.MessagePos(ctx, pipePeer, m1.ID, at(1).UnixMilli()); ok {
		t.Fatal("a message has a position at another time")
	}
	if _, err := ps.TurnPos(ctx, pipeOth, t1); !errors.Is(err, pipe.ErrNotFound) {
		t.Fatalf("TurnPos of another peer = %v", err)
	}
}

func TestPipeStoreRequestIDFindsTheMessageOnce(t *testing.T) {
	ctx := context.Background()
	ps := openTest(t).Pipe()
	m, _ := ps.AppendMessage(ctx, pipe.Message{Peer: pipePeer, Text: "hi", At: pipeT0})
	if _, ok, err := ps.MessageByRequestID(ctx, pipePeer, "r-1"); ok || err != nil {
		t.Fatalf("before: %v, %v", ok, err)
	}
	if err := ps.SetMessageRequestID(ctx, m.ID, "r-1"); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := ps.MessageByRequestID(ctx, pipePeer, "r-1"); err != nil || !ok || got.ID != m.ID || got.Text != "hi" {
		t.Fatalf("after: %+v, %v, %v", got, ok, err)
	}
	if _, ok, _ := ps.MessageByRequestID(ctx, pipeOth, "r-1"); ok {
		t.Fatal("the request id of another peer matched")
	}
	// The same id cannot name two messages of one peer.
	m2, _ := ps.AppendMessage(ctx, pipe.Message{Peer: pipePeer, Text: "again", At: pipeT0})
	if err := ps.SetMessageRequestID(ctx, m2.ID, "r-1"); err == nil {
		t.Fatal("a request id named two messages")
	}
}

// The position follows the commit, not the clock: an entry committed after the
// one a reader holds as its cursor is after it, in the same millisecond too,
// and a clock that steps back changes nothing.
func TestPipeStorePositionIsTheCommitOrderEvenInTheSameMillisecond(t *testing.T) {
	ctx := context.Background()
	ps := openTest(t).Pipe()
	same := pipeT0
	tid := turnID(t, same)
	if _, err := ps.PutTurn(ctx, pipe.Turn{TurnID: tid, Peer: pipePeer, Text: "reply", At: same}); err != nil {
		t.Fatal(err)
	}
	cursor, err := ps.TurnPos(ctx, pipePeer, tid)
	if err != nil {
		t.Fatal(err)
	}
	// A message of the same millisecond, committed after the turn the reader
	// holds, and one whose clock is an hour behind.
	late, _ := ps.AppendMessage(ctx, pipe.Message{Peer: pipePeer, Text: "same ms", At: same})
	back, _ := ps.AppendMessage(ctx, pipe.Message{Peer: pipePeer, Text: "clock stepped back", At: same.Add(-time.Hour)})
	es, err := ps.EntriesAfterPos(ctx, pipePeer, cursor, 0, true)
	if err != nil || len(es) != 2 || es[0].Message == nil || es[0].Message.ID != late.ID || es[1].Message == nil || es[1].Message.ID != back.ID {
		t.Fatalf("entries after the cursor = %+v, %v; want the two later messages in commit order", es, err)
	}
	// withTurns false leaves the turns out; a duplicate commit takes no position.
	if _, err := ps.PutTurn(ctx, pipe.Turn{TurnID: tid, Peer: pipePeer, Text: "again", At: same}); err != nil {
		t.Fatal(err)
	}
	all, _ := ps.EntriesAfterPos(ctx, pipePeer, PipePos{}, 0, false)
	if len(all) != 2 || all[0].Turn != nil || all[1].Turn != nil {
		t.Fatalf("messages only = %+v", all)
	}
	all, _ = ps.EntriesAfterPos(ctx, pipePeer, PipePos{}, 2, true)
	if len(all) != 2 || all[0].Turn == nil || all[1].Message == nil || all[1].Message.ID != late.ID {
		t.Fatalf("first page = %+v", all)
	}
	if !all[0].Pos.Less(all[1].Pos) {
		t.Fatal("positions do not grow")
	}
}

func TestPipeStoreKeepsTheAnswerOfASend(t *testing.T) {
	ctx := context.Background()
	ps := openTest(t).Pipe()
	m, _ := ps.AppendMessage(ctx, pipe.Message{Peer: pipePeer, Text: "hi", At: pipeT0})
	if res, err := ps.MessageResult(ctx, m.ID); err != nil || res != nil {
		t.Fatalf("a pending send has result %q, %v", res, err)
	}
	if err := ps.SetMessageResult(ctx, m.ID, []byte(`{"outcome":"delivered"}`)); err != nil {
		t.Fatal(err)
	}
	if res, err := ps.MessageResult(ctx, m.ID); err != nil || string(res) != `{"outcome":"delivered"}` {
		t.Fatalf("result = %q, %v", res, err)
	}
	if res, err := ps.MessageResult(ctx, m.ID+100); err != nil || res != nil {
		t.Fatalf("an unknown message has result %q, %v", res, err)
	}
}

// 0015 gives the rows of the earlier schema the order they had (Hub time,
// message before turn, then id) and the counter goes on after them.
func TestPipeMigrationBackfillsThePositionInTheOldOrder(t *testing.T) {
	ctx := context.Background()
	read := func(name string) string {
		b, err := hubMigrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	dir := t.TempDir()
	s, err := Open(dir, testMigrations(map[string]string{"0010_pipe.sql": read("0010_pipe.sql")}))
	if err != nil {
		t.Fatal(err)
	}
	// Old order: m1@1000, turn1@1000 (a turn after the message of its
	// millisecond), turn2@900 (earliest), m2@1100.
	for _, q := range []string{
		`INSERT INTO pipe_messages (worker_id, session_name, text, at) VALUES ('w-1','minion-1','m1',1000)`,
		`INSERT INTO pipe_turns (turn_id, worker_id, session_name, text, at) VALUES ('t1','w-1','minion-1','t1',1000)`,
		`INSERT INTO pipe_turns (turn_id, worker_id, session_name, text, at) VALUES ('t2','w-1','minion-1','t2',900)`,
		`INSERT INTO pipe_messages (worker_id, session_name, text, at) VALUES ('w-1','minion-1','m2',1100)`,
	} {
		if _, err := s.DB().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()

	s, err = Open(dir, testMigrations(map[string]string{
		"0010_pipe.sql":            read("0010_pipe.sql"),
		"0015_pipe_commit_pos.sql": read("0015_pipe_commit_pos.sql"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	es, err := s.Pipe().EntriesAfterPos(ctx, pipePeer, PipePos{}, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range es {
		if e.Message != nil {
			got = append(got, e.Message.Text)
		} else {
			got = append(got, e.Turn.Text)
		}
	}
	if want := "t2 m1 t1 m2"; len(got) != 4 || got[0]+" "+got[1]+" "+got[2]+" "+got[3] != want {
		t.Fatalf("order after the migration = %v, want %s", got, want)
	}
	nm, _ := s.Pipe().AppendMessage(ctx, pipe.Message{Peer: pipePeer, Text: "new", At: time.UnixMilli(1)})
	last, _ := s.Pipe().EntriesAfterPos(ctx, pipePeer, es[3].Pos, 0, true)
	if len(last) != 1 || last[0].Message == nil || last[0].Message.ID != nm.ID {
		t.Fatalf("a new message is not after the migrated entries: %+v", last)
	}
}

// Writers commit messages and turns while a reader pages through them: every
// entry is read once, in commit order, whatever interleaves. The page is one
// statement; two statements (messages, then turns) could return a later turn
// without an earlier message committed in between and lose the message.
func TestPipeStoreLivePagingLosesAndRepeatsNothing(t *testing.T) {
	ctx := context.Background()
	ps := openTest(t).Pipe()
	const each = 120
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < each; i++ {
			if _, err := ps.AppendMessage(ctx, pipe.Message{Peer: pipePeer, Text: "m", At: pipeT0}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < each; i++ {
			if _, err := ps.PutTurn(ctx, pipe.Turn{TurnID: turnID(t, pipeT0.Add(time.Duration(i)*time.Millisecond)), Peer: pipePeer, Text: "t", At: pipeT0}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	var (
		pos      PipePos
		messages int
		turns    int
	)
	deadline := time.Now().Add(30 * time.Second)
	for messages+turns < 2*each && time.Now().Before(deadline) {
		es, err := ps.EntriesAfterPos(ctx, pipePeer, pos, 7, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if !pos.Less(e.Pos) {
				t.Fatalf("position %d is not after the cursor %d", e.Pos.Commit, pos.Commit)
			}
			pos = e.Pos
			if e.Message != nil {
				messages++
			} else {
				turns++
			}
		}
	}
	wg.Wait()
	if messages != each || turns != each {
		t.Fatalf("the reader saw %d messages and %d turns, want %d of each", messages, turns, each)
	}
	if es, _ := ps.EntriesAfterPos(ctx, pipePeer, pos, 0, true); len(es) != 0 {
		t.Fatalf("%d entries after the last one", len(es))
	}
}

// 0016 keeps the volume of the turns that were stored before it, counts them
// for their Worker and the Hub, and the triggers keep the sums from then on:
// an insert adds and a delete (retention) gives back.
func TestPipeTurnQuotaMigrationSeedsAndTracksTheUsage(t *testing.T) {
	ctx := context.Background()
	read := func(name string) string {
		b, err := hubMigrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	old := map[string]string{"0010_pipe.sql": read("0010_pipe.sql"), "0015_pipe_commit_pos.sql": read("0015_pipe_commit_pos.sql")}
	dir := t.TempDir()
	s, err := Open(dir, testMigrations(old))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO pipe_turns (turn_id, worker_id, session_name, text, at) VALUES ('t1','w-1','minion-1','abcd',1000)`,
		`INSERT INTO pipe_turns (turn_id, worker_id, session_name, text, at) VALUES ('t2','w-1','minion-1','ef',900)`,
		`INSERT INTO pipe_turns (turn_id, worker_id, session_name, text, at) VALUES ('t3','w-2','minion-1','g',900)`,
	} {
		if _, err := s.DB().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()

	old["0016_pipe_turn_quota.sql"] = read("0016_pipe_turn_quota.sql")
	s, err = Open(dir, testMigrations(old))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ps := s.Pipe()
	check := func(what, scope, key string, turns, cost int64) {
		t.Helper()
		if u, err := ps.TurnUsage(ctx, scope, key); err != nil || u.Turns != turns || u.Cost != cost {
			t.Fatalf("%s: usage = %+v, %v; want %d turns, cost %d", what, u, err, turns, cost)
		}
	}
	check("seed worker 1", "worker", "w-1", 2, TurnCost(4)+TurnCost(2))
	check("seed worker 2", "worker", "w-2", 1, TurnCost(1))
	check("seed global", "global", "", 3, TurnCost(4)+TurnCost(2)+TurnCost(1))

	// A delivered turn adds to its Worker, its owner and the Hub, atomically
	// with the insert.
	m, err := ps.AppendMessage(ctx, pipe.Message{Peer: pipe.Peer{WorkerID: "w-1", SessionName: "minion-1"}, Text: "q", At: time.UnixMilli(2000)})
	if err != nil {
		t.Fatal(err)
	}
	put := func(id string, msg int64, w, owner string, lim TurnLimits) TurnStatus {
		st, err := ps.PutDeliveredTurn(ctx, DeliveredTurn{
			Turn:      pipe.Turn{TurnID: id, Peer: pipe.Peer{WorkerID: w, SessionName: "minion-1"}, Text: "hello", At: time.UnixMilli(2000)},
			MessageID: msg, OwnerID: owner, Limits: lim,
		})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := put("t4", m.ID, "w-1", "o-1", TurnLimits{}); st != TurnStored {
		t.Fatalf("status = %v", st)
	}
	check("owner", "owner", "o-1", 1, TurnCost(5))
	check("worker 1", "worker", "w-1", 3, TurnCost(4)+TurnCost(2)+TurnCost(5))
	// The same turn again is a duplicate and changes nothing; another turn for
	// the delivery, or for a delivery of another Worker, is refused.
	if st := put("t4", m.ID, "w-1", "o-1", TurnLimits{}); st != TurnDuplicate {
		t.Fatalf("repeat = %v", st)
	}
	if st := put("t5", m.ID, "w-1", "o-1", TurnLimits{}); st != TurnAnswered {
		t.Fatalf("second reply = %v", st)
	}
	if st := put("t6", m.ID, "w-2", "o-2", TurnLimits{}); st != TurnNoDelivery {
		t.Fatalf("other Worker's delivery = %v", st)
	}
	if st := put("t4", m.ID, "w-2", "o-2", TurnLimits{}); st != TurnIDTaken {
		t.Fatalf("taken id = %v", st)
	}
	check("worker 1 unchanged", "worker", "w-1", 3, TurnCost(4)+TurnCost(2)+TurnCost(5))
	// A quota refuses before the insert.
	m2, _ := ps.AppendMessage(ctx, pipe.Message{Peer: pipe.Peer{WorkerID: "w-1", SessionName: "minion-1"}, Text: "q", At: time.UnixMilli(2001)})
	if st := put("t7", m2.ID, "w-1", "o-1", TurnLimits{OwnerCost: TurnCost(5)}); st != TurnQuotaOwner {
		t.Fatalf("owner quota = %v", st)
	}
	if st := put("t7", m2.ID, "w-1", "o-1", TurnLimits{GlobalTurns: 4}); st != TurnQuotaGlobal {
		t.Fatalf("global quota = %v", st)
	}
	// Retention gives the volume back.
	if _, turns, err := ps.DeleteBefore(ctx, time.UnixMilli(1500)); err != nil || turns != 2+1 {
		t.Fatalf("DeleteBefore = %d, %v", turns, err)
	}
	check("worker 1 after prune", "worker", "w-1", 1, TurnCost(5))
	check("global after prune", "global", "", 1, TurnCost(5))
	// Pressure reads sizes.
	if p, err := ps.Pressure(ctx, 1<<30); err != nil || p.DBBytes <= 0 {
		t.Fatalf("Pressure = %+v, %v", p, err)
	}
}
