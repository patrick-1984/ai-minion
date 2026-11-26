package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/claudeallow"
	"github.com/patrick-1984/ai-minion/internal/mcp"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

const (
	gateLocalWorker = "01890a5d-ac96-774b-bcce-b30209900001"
	gateHubWorker   = "01890a5d-ac96-774b-bcce-b30209900002"
)

func writeGateList(t *testing.T, path, worker string) {
	t.Helper()
	f := &claudeallow.File{Version: claudeallow.Version, Rules: []claudeallow.Rule{{
		ID: ops.NewUUIDv7(), Created: time.Now().UTC(), Ops: []string{ops.OpSendText},
		Targets: []claudeallow.Target{{WorkerID: worker}},
	}}}
	b, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func sendTextCall(session string) string {
	return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"send_text","arguments":{"target":{"session_name":"` + session + `"},"text":"x"}}}`
}

// In Solo the local control socket runs a target without a worker_id on the
// local Worker, so a rule for the --hub-worker must not reach it; once the
// last rule is gone, write tools that exist only for rules refuse every write.
func TestMCPGateJudgesSoloTargetsOnTheLocalWorkerAndFailsClosedAfterRevoke(t *testing.T) {
	list := filepath.Join(t.TempDir(), claudeallow.FileName)
	writeGateList(t, list, gateHubWorker)
	b := &stubBackend{out: map[string]string{
		agent.OpLocalStatus: `{"state_dir":"/s","state":"connected","worker_id":"` + gateLocalWorker + `","hub_preview_only":false,"pairing_known":false}`,
		ops.OpSendText:      `{"outcome":"delivered","reason":"","forceable":false,"forced":false,"warnings":[],"pane":null}`,
	}}
	deps, _ := testMCPDeps(map[string]string{"MINION_HUB_WORKER": gateHubWorker}, b, mcp.TrustAll)
	deps.allowlist = list

	got, _ := rpc(t, deps, nil, sendTextCall("my-local"))
	if !strings.Contains(string(got[2]), "not in a standing Minion allow rule") {
		t.Fatalf("a local session under a rule of the Hub Worker: %s", got[2])
	}
	if slices.Contains(b.ops(), ops.OpSendText) {
		t.Fatal("the refused write reached the backend")
	}

	writeGateList(t, list, gateLocalWorker)
	got, _ = rpc(t, deps, nil, sendTextCall("my-local"))
	if strings.Contains(string(got[2]), `"isError":true`) || !slices.Contains(b.ops(), ops.OpSendText) {
		t.Fatalf("a local session under a rule of the local Worker: %s", got[2])
	}

}

func TestMCPWritesOnlyForRulesFailClosedAfterTheLastRevoke(t *testing.T) {
	list := filepath.Join(t.TempDir(), claudeallow.FileName)
	in := json.RawMessage(`{"target":{"worker_id":"` + gateLocalWorker + `","session_name":"s"},"text":"x"}`)
	sendText, _ := ops.Lookup(ops.OpSendText)
	if allow, _ := mcpWrites(false, claudeallow.Gate{Path: list}); allow {
		t.Fatal("write tools without --allow-writes and without an allowlist")
	}
	writeGateList(t, list, gateLocalWorker)
	allow, gate := mcpWrites(false, claudeallow.Gate{Path: list})
	if !allow || !gate.Required {
		t.Fatalf("write tools for the rules: allow=%v gate=%+v", allow, gate)
	}
	if _, err := gate.Check(sendText, in); err != nil {
		t.Fatalf("a write under the rule: %v", err)
	}
	if err := os.Remove(list); err != nil { // minion claude revoke --all
		t.Fatal(err)
	}
	if _, err := gate.Check(sendText, in); err == nil || !strings.Contains(err.Error(), "no standing Minion allow rule is left") {
		t.Fatalf("a write after the last revoke: %v", err)
	}
	// --allow-writes keeps the behaviour without an allowlist.
	allow, gate = mcpWrites(true, claudeallow.Gate{Path: list})
	if !allow || gate.Required {
		t.Fatalf("--allow-writes: allow=%v gate=%+v", allow, gate)
	}
	if _, err := gate.Check(sendText, in); err != nil {
		t.Fatalf("--allow-writes without an allowlist: %v", err)
	}
}

func TestMCPGateUsesTheHubWorkerOnlyWithAHub(t *testing.T) {
	list := filepath.Join(t.TempDir(), claudeallow.FileName)
	deps, _ := testMCPDeps(map[string]string{"MINION_HUB_WORKER": gateHubWorker, "MINION_HUB_URL": "https://hub.invalid"}, &stubBackend{}, mcp.TrustAll)
	deps.allowlist = list
	cmd := newMCPCmd(deps)
	g, err := mcpGate(cmd, deps, &stubBackend{})
	if err != nil {
		t.Fatal(err)
	}
	if g.DefaultWorker != gateHubWorker || g.LocalWorker != nil {
		t.Fatalf("Hub gate = %+v", g)
	}
}

func TestLocalWorkerIDIsAskedUntilKnownThenKept(t *testing.T) {
	b := &stubBackend{out: map[string]string{agent.OpLocalStatus: `{"state":"connecting"}`}}
	id := localWorkerID(b)
	if _, err := id(); err == nil {
		t.Fatal("a Worker without an id gave one")
	}
	b.mu.Lock()
	b.out[agent.OpLocalStatus] = `{"worker_id":"` + gateLocalWorker + `"}`
	b.mu.Unlock()
	for range 2 {
		if got, err := id(); err != nil || got != gateLocalWorker {
			t.Fatalf("id = %q, %v", got, err)
		}
	}
	if n := len(b.ops()); n != 2 {
		t.Fatalf("status asked %d times, want 2", n)
	}
}

// The allowlist is one file for every Claude Code profile, so its lock is
// next to it: an allow of another profile waits for it.
func TestClaudeAllowLocksTheSharedAllowlist(t *testing.T) {
	e := newClaudeTestEnv(t)
	if err := os.MkdirAll(filepath.Dir(e.list), 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := lockConfigFileContext(context.Background(), claudeallow.LockPath(e.list))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.run(t, "allow", "--worker", claudeTestWorker, "--yes")
		done <- err
	}()
	select {
	case err := <-done:
		release()
		t.Fatalf("allow did not wait for the allowlist lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("allow did not finish after the lock was released")
	}
	list, err := claudeallow.Load(e.list)
	if err != nil || list == nil || len(list.Rules) != 1 {
		t.Fatalf("allowlist = %+v, %v", list, err)
	}
}
