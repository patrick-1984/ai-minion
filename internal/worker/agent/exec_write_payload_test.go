package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// An identical pipe_send retry gets a fresh message_id from the Hub (a v0.21.0
// Hub deletes its broker row after failed_not_sent and stores a new one). The
// Worker answers the retry from its cache with the original refusal, never a
// conflict, and writes nothing.
func TestPipeSendRetryWithAFreshMessageIDGetsTheCachedRefusal(t *testing.T) {
	p := newPipeEnv(t)
	p.ah.addSession("sh", "bash")
	p.cls.set("bash", ops.PaneKindShell, ops.PaneStateStable)

	args := pipeSendArgs("sh", "ls", false)
	args.MessageID = 7
	first, err := p.hubSend(operateActions, args)
	if err != nil || first.Outcome != ops.OutcomeFailedNotSent || first.Reason != "shell_pane" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	retry := args
	retry.MessageID = 8
	again, err := p.hubSend(operateActions, retry)
	if err != nil {
		t.Fatalf("an identical retry with a new message_id must not conflict: %v", err)
	}
	if again.Outcome != first.Outcome || again.Reason != first.Reason {
		t.Fatalf("retry = %+v, want the cached %+v", again, first)
	}
	if len(p.host.pastes()) != 0 {
		t.Fatalf("a refused send wrote: %v", p.host.pastes())
	}
	// Other text under the same request id is still another request.
	other := args
	other.MessageID = 9
	other.Text = "rm -rf ."
	_, err = p.hubSend(operateActions, other)
	wantAPIError(t, err, "conflict", "")
}

// The digest is taken of the arguments the handler executes. A member spelled
// twice with different case decodes to the value of the last spelling, so two
// payloads that differ only in that order are two different writes and must
// never share a cached answer.
func TestRequestIDWithDifferentlyCasedMembersInOtherOrderConflicts(t *testing.T) {
	e, _, _ := newTargetEnv(t)
	id := ops.NewUUIDv7()
	first := json.RawMessage(`{"request_id":"` + id + `","target":{"session_name":"work"},"text":"harmless","Text":"effective"}`)
	if _, err := e.callRaw(userOrigin("user-a"), ops.OpSendText, first); err != nil {
		t.Fatal(err)
	}
	got := e.host.pastes()
	if len(got) != 1 || got[0].Text != "effective" {
		t.Fatalf("pastes = %+v, want the last spelling", got)
	}
	swapped := json.RawMessage(`{"request_id":"` + id + `","target":{"session_name":"work"},"Text":"effective","text":"harmless"}`)
	_, err := e.callRaw(userOrigin("user-a"), ops.OpSendText, swapped)
	wantAPIError(t, err, "conflict", "")
	if n := len(e.host.pastes()); n != 1 {
		t.Fatalf("pastes = %d, want 1", n)
	}
}

func TestWritePayloadHashFollowsTheTypedArguments(t *testing.T) {
	hash := func(op, raw string) string {
		o, ok := ops.Lookup(op)
		if !ok {
			t.Fatalf("op %s is not registered", op)
		}
		return writePayloadHash(withWritePayload(context.Background(), json.RawMessage(raw)), o)
	}
	a := hash(ops.OpSendText, `{"request_id":"r","target":{"session_name":"w"},"text":"x","submit":true}`)
	b := hash(ops.OpSendText, `{ "submit": true, "text": "x", "target": {"session_name": "w"}, "request_id": "r" }`)
	if a != b || len(a) != 64 {
		t.Fatalf("one request in two encodings: %q vs %q", a, b)
	}
	if c := hash(ops.OpSendText, `{"request_id":"r","target":{"session_name":"w"},"text":"y","submit":true}`); c == a {
		t.Fatal("other text has the same digest")
	}
	m7 := hash(ops.OpPipeSend, `{"request_id":"r","target":{"session_name":"w"},"text":"x","message_id":7}`)
	m8 := hash(ops.OpPipeSend, `{"request_id":"r","target":{"session_name":"w"},"text":"x","message_id":8}`)
	if m7 != m8 {
		t.Fatal("the Hub-assigned message_id changes the digest")
	}
	// Arguments that do not decode are still digested, and differ.
	if x, y := hash(ops.OpSendText, `{"text":1}`), hash(ops.OpSendText, `{"text":2}`); x == "" || x == y {
		t.Fatalf("undecodable arguments: %q %q", x, y)
	}
	if got := writePayloadHash(context.Background(), ops.Op{}); got != "" {
		t.Fatalf("no payload: %q", got)
	}
}
