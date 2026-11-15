package ops

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

func TestLocalRequiredFeaturesNeedTheForwardWhenAWorkerIsNamed(t *testing.T) {
	w := NewUUIDv7()
	tgt := func(worker string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"target": Target{WorkerID: worker, SessionName: "x"}, "text": "plain"})
		return b
	}
	for _, tc := range []struct {
		op   string
		args json.RawMessage
		want bool
	}{
		{OpSendText, tgt(w), true},
		{OpSendKeys, tgt(w), true},
		{OpSessionSnapshot, tgt(w), true},
		{OpSetTask, tgt(w), true},
		{OpSessionsList, json.RawMessage(`{"worker_id":"` + w + `"}`), true},
		{OpSendText, tgt(""), false},
		{OpSessionsList, nil, false},
		{OpPipeSend, tgt(w), false}, // the pipe decides itself (Hub or Solo)
	} {
		got := slices.Contains(LocalRequiredFeatures(tc.op, tc.args), proto.FeatureSessionForward)
		if got != tc.want {
			t.Errorf("%s %s: needs session.forward = %v, want %v", tc.op, tc.args, got, tc.want)
		}
		if slices.Contains(RequiredFeatures(tc.op, tc.args), proto.FeatureSessionForward) {
			t.Errorf("%s: the Hub-side rule asks for the local-only feature", tc.op)
		}
	}
}
