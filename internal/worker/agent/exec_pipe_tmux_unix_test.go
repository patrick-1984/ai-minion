//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/fakeagent"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// agentClassifier calls every pane an idle aider, the agent whose screen
// markers the fake agent follows (Story 7.2); the extractor classifies the
// frames itself.
type agentClassifier struct{}

func (agentClassifier) Classify(context.Context, agentdetect.Input) agentdetect.Result {
	return agentdetect.Result{Kind: ops.PaneKindAgentPrefix + "aider", State: ops.PaneStateStable}
}

// The story's integration test: pipe_send through the local socket to a fake
// agent in a real tmux pane, then pipe_wait returns the turn. It needs tmux
// 3.8 for the turn (EPICS A-18) and runs on the tmux 3.8 job; on an older tmux
// it expects the message delivered, the reason tmux_too_old and no turn.
func TestTmuxSoloPipeSendThenPipeWaitOverTheLocalSocket(t *testing.T) {
	srv := tmuxtest.New(t)
	host, err := tmux.New(srv.Socket())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctl, out := filepath.Join(dir, "ctl"), filepath.Join(dir, "out")
	for _, f := range []string{ctl, out} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	paneTarget := srv.NewSession("work", fakeagent.Argv(fakeagent.ModeLine, ctl, out)...)
	srv.WaitFor(paneTarget, ">", 10*time.Second)

	st, err := store.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveShare(store.ShareSelection{Mode: store.ShareAll}); err != nil {
		t.Fatal(err)
	}
	ex, err := NewExecutor(Config{Host: host, Classifier: agentClassifier{}, State: st, Policy: NewPolicy(st), LockWait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewPipeService(PipeConfig{
		Executor: ex, Store: st, Hub: &fakePipeHub{}, Poll: 50 * time.Millisecond, WaitPoll: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })

	// The local socket of the real module, in front of this executor and pipe.
	c := serveLocalPipe(t, ex, svc, st)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	raw, err := c.Call(ctx, ops.OpPipeSend, ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target("work"), Text: "hello agent"})
	if err != nil {
		t.Fatal(err)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(raw, &res); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("pipe_send = %s (%v)", raw, err)
	}
	waitFile(t, out, `submitted "hello agent"`)

	raw, err = c.Call(ctx, ops.OpPipeRead, ops.PipeReadArgs{Target: target("work")})
	var read ops.PipeReadResult
	if err != nil || json.Unmarshal(raw, &read) != nil || len(read.Turns) != 1 || read.Turns[0].Direction != ops.PipeSent || read.Turns[0].Text != "hello agent" {
		t.Fatalf("pipe_read = %s (%v)", raw, err)
	}
	cursor := read.Turns[0].TurnID

	appendFile(t, ctl, "rows 3", "done")
	var got []ops.PipeTurn
	timeout := 30000
	if !host.TurnsSupported() {
		timeout = 1500
	}
	_, err = c.Stream(ctx, ops.OpPipeWait, ops.PipeWaitArgs{Target: target("work"), AfterTurnID: cursor, TimeoutMS: timeout},
		func(m json.RawMessage) error {
			var pt ops.PipeTurn
			if err := json.Unmarshal(m, &pt); err != nil {
				return err
			}
			got = append(got, pt)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}

	if !host.TurnsSupported() {
		t.Logf("tmux %s: no row counters, so no turn (reason tmux_too_old)", host.Version())
		if len(got) != 0 {
			t.Fatalf("a turn on a tmux without counters: %+v", got)
		}
		var warned bool
		for _, w := range res.Warnings {
			warned = warned || (w.Code == "turn_unavailable" && strings.Contains(w.Message, agentdetect.ReasonTmuxTooOld))
		}
		if !warned {
			t.Fatalf("the sender is not told: warnings = %+v", res.Warnings)
		}
		// The message was delivered once and never written again.
		if n := strings.Count(readTestFile(out), `submitted "hello agent"`); n != 1 {
			t.Fatalf("the message was submitted %d times", n)
		}
		return
	}
	if len(got) != 1 || got[0].Text != "r1\nr2\nr3" || got[0].Direction != ops.PipeReceived || got[0].Session != "work" {
		t.Fatalf("pipe_wait = %+v, want the turn r1..r3", got)
	}
	raw, err = c.Call(ctx, ops.OpPipeRead, ops.PipeReadArgs{Target: target("work")})
	if err != nil || json.Unmarshal(raw, &read) != nil || len(read.Turns) != 2 || read.Turns[1].TurnID != got[0].TurnID {
		t.Fatalf("pipe_read after the turn = %s (%v)", raw, err)
	}
	// The turn is stored once, and nothing was written twice.
	if n := strings.Count(readTestFile(out), `submitted "hello agent"`); n != 1 {
		t.Fatalf("the message was submitted %d times", n)
	}
}

func readTestFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func waitFile(t *testing.T, path, substring string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(readTestFile(path), substring) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never held %q: %q", path, substring, readTestFile(path))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func appendFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}
