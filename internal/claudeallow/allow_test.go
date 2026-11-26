package claudeallow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const (
	workerA = "01890a5d-ac96-774b-bcce-b302099a8057"
	workerB = "01890a5d-ac96-774b-bcce-b302099a8058"
)

func writeList(t *testing.T, f *File) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	b, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func sampleRule() Rule {
	return Rule{ID: ops.NewUUIDv7(), Created: time.Now().UTC(), Ops: []string{ops.OpSendText, ops.OpSessionSnapshot},
		Targets: []Target{{WorkerID: workerA, Sessions: []string{"shell"}}, {WorkerID: workerB}}}
}

func op(t *testing.T, name string) ops.Op {
	t.Helper()
	o, ok := ops.Lookup(name)
	if !ok {
		t.Fatal(name)
	}
	return o
}

func TestMatchLimitsOpsWorkersAndSessions(t *testing.T) {
	r := sampleRule()
	f := &File{Version: Version, Rules: []Rule{r}}
	for _, c := range []struct {
		op, worker, session string
		want                bool
	}{
		{ops.OpSendText, workerA, "shell", true},
		{ops.OpSendText, workerA, "other", false},
		{ops.OpSendText, workerA, "", false},
		{ops.OpSendText, workerB, "anything", true},
		{ops.OpSendKeys, workerB, "anything", false},
		{ops.OpSendText, "", "shell", false},
	} {
		if _, ok := f.Match(c.op, c.worker, c.session); ok != c.want {
			t.Errorf("Match(%s, %s, %q) = %v, want %v", c.op, c.worker, c.session, ok, c.want)
		}
	}
}

func TestGateWithoutAllowlistPassesEverything(t *testing.T) {
	g := Gate{Path: filepath.Join(t.TempDir(), FileName)}
	rule, err := g.Check(op(t, ops.OpSendText), json.RawMessage(`{"target":{"session_name":"x"}}`))
	if err != nil || rule != "" {
		t.Fatalf("Check = %q, %v", rule, err)
	}
	if g.Active() {
		t.Error("a missing allowlist is active")
	}
}

func TestGateRefusesWritesOutsideEveryRuleAndRereadsTheFile(t *testing.T) {
	r := sampleRule()
	path := writeList(t, &File{Version: Version, Rules: []Rule{r}})
	g := Gate{Path: path}
	if !g.Active() {
		t.Fatal("not active")
	}
	in := json.RawMessage(`{"target":{"worker_id":"` + workerA + `","session_name":"shell"},"text":"x"}`)
	rule, err := g.Check(op(t, ops.OpSendText), in)
	if err != nil || rule != r.ID {
		t.Fatalf("allowed call: %q, %v", rule, err)
	}
	out := json.RawMessage(`{"target":{"worker_id":"` + workerA + `","session_name":"root"},"text":"x"}`)
	if _, err := g.Check(op(t, ops.OpSendText), out); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("outside call: %v", err)
	}
	// Reads are never refused.
	if _, err := g.Check(op(t, ops.OpSessionCapture), out); err != nil {
		t.Fatalf("read: %v", err)
	}
	// A revoke that leaves an empty list refuses at the very next call.
	empty := &File{Version: Version}
	b, _ := empty.Encode()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Check(op(t, ops.OpSendText), in); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("after revoke: %v", err)
	}
}

func TestGateDefaultWorkerFillsATargetWithoutOne(t *testing.T) {
	r := sampleRule()
	g := Gate{Path: writeList(t, &File{Version: Version, Rules: []Rule{r}}), DefaultWorker: workerA}
	if _, err := g.Check(op(t, ops.OpSendText), json.RawMessage(`{"target":{"session_name":"shell"}}`)); err != nil {
		t.Fatal(err)
	}
}

func TestGateFailsClosedOnABrokenAllowlist(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(`{"version":1,"rules":[{"id":"x"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	g := Gate{Path: path}
	if _, err := g.Check(op(t, ops.OpSendText), json.RawMessage(`{}`)); err == nil {
		t.Fatal("a broken allowlist allowed a write")
	}
	if g.Active() {
		t.Error("a broken allowlist is active")
	}
}

func TestToolEntryNames(t *testing.T) {
	if got := ToolEntry("minion", "send_text"); got != "mcp__minion__send_text" {
		t.Error(got)
	}
	if got := ToolEntry(PluginServer("minion-control", "minion"), "send_keys"); got != "mcp__plugin_minion-control_minion__send_keys" {
		t.Error(got)
	}
	if got := ToolEntry("my.server", "x"); got != "mcp__my_server__x" {
		t.Error(got)
	}
}
