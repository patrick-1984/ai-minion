package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// A duplicate request id with the same body is answered from the cache and
// says so; the text is sent once (A49 T14).
func TestDuplicateWithSameBodyIsReplayed(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	args := sendText("work", "hello", false)
	first := e.mustWrite(ops.OpSendText, args)
	if first.Replayed || first.Outcome != ops.OutcomeDelivered {
		t.Fatalf("first = %+v", first)
	}
	again := e.mustWrite(ops.OpSendText, args)
	if !again.Replayed || again.Outcome != ops.OutcomeDelivered || len(e.host.pastes()) != 1 {
		t.Fatalf("duplicate = %+v, %d pastes", again, len(e.host.pastes()))
	}
}

// A retry with different text is refused, not reported as delivered.
func TestDuplicateWithOtherBodyIsAConflict(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	args := sendText("work", "hello", false)
	e.mustWrite(ops.OpSendText, args)
	other := args
	other.Text = "something else"
	_, err := e.write(ops.OpSendText, other)
	ae := apiErrorOf(t, err, "conflict", "")
	if ae.Message != "this request_id was already used for a different request" {
		t.Fatalf("message = %q", ae.Message)
	}
	if len(e.host.pastes()) != 1 {
		t.Fatal("the other text was sent")
	}
}

// An unknown cached result is replayed as unknown with the flag.
func TestDuplicateOfUnknownResultIsReplayedUnknown(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	e.host.pasteErr = errors.New("tmux exploded")
	args := sendText("work", "x", false)
	first := e.mustWrite(ops.OpSendText, args)
	again := e.mustWrite(ops.OpSendText, args)
	if first.Outcome != ops.OutcomeUnknown || again.Outcome != ops.OutcomeUnknown || !again.Replayed || first.Replayed {
		t.Fatalf("first %+v again %+v", first, again)
	}
}

// A row without a digest (written by an older Worker) matches as before.
func TestCachedRowWithoutDigestMatchesAsBefore(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	args := sendText("work", "hello", false)
	e.mustWrite(ops.OpSendText, args)
	// Blank the digest as an old row would have it.
	rec, ok, err := e.st.GetResult(context.Background(), args.RequestID)
	if err != nil || !ok || rec.ArgsSHA256 == "" {
		t.Fatalf("record %+v %v %v", rec, ok, err)
	}
	rec.ArgsSHA256 = ""
	if err := e.st.ReplaceResult(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	other := args
	other.Text = "different"
	res, err := e.write(ops.OpSendText, other)
	if err != nil || !res.Replayed {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestArgsDigestIgnoresRequestIDAndKeyOrder(t *testing.T) {
	a := ArgsDigest(json.RawMessage(`{"request_id":"a","text":"x","session":"s"}`))
	b := ArgsDigest(json.RawMessage(`{"session":"s","text":"x","request_id":"b"}`))
	c := ArgsDigest(json.RawMessage(`{"session":"s","text":"y","request_id":"a"}`))
	if a == "" || a != b || a == c {
		t.Fatalf("digests %q %q %q", a, b, c)
	}
	if ArgsDigest(json.RawMessage(`not json`)) != "" {
		t.Fatal("garbage must give no digest")
	}
}

func TestWriteResultReplayedJSON(t *testing.T) {
	plain, _ := json.Marshal(ops.WriteResult{Outcome: ops.OutcomeDelivered})
	if string(plain) != `{"outcome":"delivered","reason":"","forceable":false,"forced":false,"warnings":[],"pane":null}` {
		t.Fatalf("plain = %s", plain)
	}
	rep, _ := json.Marshal(ops.WriteResult{Outcome: ops.OutcomeDelivered, Replayed: true})
	if string(rep) != `{"outcome":"delivered","reason":"","forceable":false,"forced":false,"warnings":[],"pane":null,"replayed":true}` {
		t.Fatalf("replayed = %s", rep)
	}
}
