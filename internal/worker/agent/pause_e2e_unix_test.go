//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

func localPause(t *testing.T, w *fullWorker, pause bool) PauseResult {
	t.Helper()
	op := OpLocalResume
	if pause {
		op = OpLocalPause
	}
	raw, err := w.client.Call(context.Background(), localapi.QualifiedOp(op, []string{FeaturePause}), LocalPauseArgs{})
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	var res PauseResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// Two pause/resume cycles: each pause ends the Hub link and no dial follows
// while paused; after each resume a Hub read and a Hub write succeed.
func TestPauseResumeTwoCyclesRemoteOps(t *testing.T) {
	w := startFullWorker(t)
	hub := w.nextHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for cycle := 1; cycle <= 2; cycle++ {
		if res := localPause(t, w, true); !res.Paused || res.WritesPending != 0 {
			t.Fatalf("cycle %d: pause = %+v", cycle, res)
		}
		select {
		case <-hub.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("cycle %d: the pause did not end the Hub link", cycle)
		}
		// The ack comes after the connector settled as paused.
		if st := statusReply(t, w.client); st.Paused == nil || !*st.Paused || st.State != string(ConnIdle) || st.Reason != ReasonPaused {
			t.Fatalf("cycle %d: live status = %+v", cycle, st)
		}
		select {
		case <-w.dialer.hubs:
			t.Fatalf("cycle %d: the paused Worker dialed the Hub", cycle)
		case <-time.After(300 * time.Millisecond):
		}
		if res := localPause(t, w, false); res.Paused {
			t.Fatalf("cycle %d: resume = %+v", cycle, res)
		}
		hub = w.nextHub(t)
		if body := hubCall(t, ctx, hub, ops.OpSessionsList, nil); !strings.Contains(body, proto.TypeOpResult) || !strings.Contains(body, `"work"`) {
			t.Fatalf("cycle %d: sessions_list = %s", cycle, body)
		}
		args := ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{SessionName: "work"}, Text: "hi"}
		if body := hubCall(t, ctx, hub, ops.OpSendText, args); !strings.Contains(body, proto.TypeOpResult) || !strings.Contains(body, `"delivered"`) {
			t.Fatalf("cycle %d: send_text = %s", cycle, body)
		}
	}
}

// A local request that needs the Hub answers paused, not worker_offline.
func TestPausedWorkersListIsPaused(t *testing.T) {
	w := startFullWorker(t)
	_ = w.nextHub(t)
	localPause(t, w, true)
	_, err := w.client.Call(context.Background(), ops.OpWorkersList, map[string]any{})
	if proto.CodeOf(err) != CodePaused {
		t.Fatalf("workers_list while paused = %v, want code paused", err)
	}
}
