package workers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The Hub names the routed Worker outside the arguments only where the
// arguments do not already name it where the Worker checks them (review r2),
// so a frame that fit the stream limit before the member existed still fits.
func TestRoutedTargetWorkerIsStampedOnlyWhenTheArgumentsDoNotNameIt(t *testing.T) {
	const w, other = "01a00000-0000-7000-8000-000000000001", "01a00000-0000-7000-8000-000000000002"
	for _, tc := range []struct {
		name, op, args, want string
	}{
		{"target names it", ops.OpSendText, `{"target":{"worker_id":"` + w + `","session_name":"x"}}`, ""},
		{"target without a worker", ops.OpSendText, `{"target":{"session_name":"x"}}`, w},
		{"target names another Worker", ops.OpSendText, `{"target":{"worker_id":"` + other + `","session_name":"x"}}`, w},
		{"top-level id of a target op is not checked there", ops.OpSessionSnapshot, `{"worker_id":"` + w + `","target":{"session_name":"x"}}`, w},
		{"sessions_list names it", ops.OpSessionsList, `{"worker_id":"` + w + `"}`, ""},
		{"sessions_list names none", ops.OpSessionsList, `{}`, w},
		{"session_create names it", ops.OpSessionCreate, `{"worker_id":"` + w + `","name":"x"}`, ""},
		{"session_create target member is not checked there", ops.OpSessionCreate, `{"target":{"worker_id":"` + w + `"}}`, w},
		{"undecodable arguments", ops.OpSendText, `{"target":"x"}`, w},
	} {
		if got := routedTargetWorker(tc.op, []byte(tc.args), w); got != tc.want {
			t.Errorf("%s: target_worker_id = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A send_text frame that is exactly at the stream limit without the member
// stays exactly there: the typed target names the Worker.
func TestRoutedTargetWorkerKeepsAFullSendTextFrameUnchanged(t *testing.T) {
	const w = "01a00000-0000-7000-8000-000000000001"
	args := ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{WorkerID: w, SessionName: "x"}}
	frame := func(text string, stamp string) []byte {
		args.Text = text
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(OpRequest{Type: proto.TypeOpRequest, Op: ops.OpSendText, RequestID: args.RequestID, Args: raw, TargetWorkerID: stamp})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	base := len(frame("", ""))
	text := strings.Repeat("a", proto.StreamFrameLimit-base)
	before := frame(text, "")
	if len(before) != proto.StreamFrameLimit {
		t.Fatalf("fixture frame = %d bytes", len(before))
	}
	args.Text = text
	raw, _ := json.Marshal(args)
	if got := frame(text, routedTargetWorker(ops.OpSendText, raw, w)); len(got) != len(before) {
		t.Fatalf("frame grew from %d to %d bytes", len(before), len(got))
	}
}
