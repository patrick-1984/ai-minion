package claudeallow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	// A read no rule names prompts in Claude Code, so the gate lets it pass.
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

func TestGateLimitsTheReadsARuleNamesToItsTargets(t *testing.T) {
	r := sampleRule() // send_text and session_snapshot
	g := Gate{Path: writeList(t, &File{Version: Version, Rules: []Rule{r}})}
	in := json.RawMessage(`{"target":{"worker_id":"` + workerA + `","session_name":"shell"}}`)
	out := json.RawMessage(`{"target":{"worker_id":"` + workerA + `","session_name":"root"}}`)
	if rule, err := g.Check(op(t, ops.OpSessionSnapshot), in); err != nil || rule != r.ID {
		t.Fatalf("read on a rule target: %q, %v", rule, err)
	}
	if _, err := g.Check(op(t, ops.OpSessionSnapshot), out); !sameErr(err, ErrNotAllowed) {
		t.Fatalf("read outside every rule target: %v", err)
	}
	for _, name := range []string{ops.OpSessionsList, ops.OpWorkersList} {
		if _, err := g.Check(op(t, name), json.RawMessage(`{"worker_id":"`+workerA+`"}`)); err != nil {
			t.Errorf("%s names no session and must pass: %v", name, err)
		}
	}
}

func TestGateRefusesWritesWhenTheyExistOnlyForRulesAndNoneIsLeft(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	in := json.RawMessage(`{"target":{"worker_id":"` + workerA + `","session_name":"shell"},"text":"x"}`)
	if _, err := (Gate{Path: path, Required: true}).Check(op(t, ops.OpSendText), in); !sameErr(err, ErrNoRuleLeft) {
		t.Fatalf("write after the last revoke: %v", err)
	}
	if _, err := (Gate{Path: path, Required: true}).Check(op(t, ops.OpSessionSnapshot), in); err != nil {
		t.Fatalf("a read without an allowlist: %v", err)
	}
	if _, err := (Gate{Path: path}).Check(op(t, ops.OpSendText), in); err != nil {
		t.Fatalf("--allow-writes without an allowlist: %v", err)
	}
}

func TestGateRefusesASessionCodeWithoutAWorker(t *testing.T) {
	r := sampleRule() // the whole of workerB
	g := Gate{Path: writeList(t, &File{Version: Version, Rules: []Rule{r}}), DefaultWorker: workerB}
	for _, code := range []string{"5-667", "#5-667", "s5-667", "5-667:0.1"} {
		in := json.RawMessage(`{"target":{"session_name":"` + code + `"},"text":"x"}`)
		if _, err := g.Check(op(t, ops.OpSendText), in); !sameErr(err, ErrCodeTarget) {
			t.Errorf("code %s: %v", code, err)
		}
	}
	// With its Worker named, the same text is a session name.
	in := json.RawMessage(`{"target":{"worker_id":"` + workerB + `","session_name":"5-667"},"text":"x"}`)
	if _, err := g.Check(op(t, ops.OpSendText), in); err != nil {
		t.Fatalf("a named Worker: %v", err)
	}
}

// On the local socket only the local Worker runs a target without a
// worker_id, and it never resolves a session code, so a name that looks like
// one ("2024-11") is judged as an ordinary session of that Worker.
func TestGateTakesACodeLikeNameOnTheLocalSocketAsASession(t *testing.T) {
	r := sampleRule() // the whole of workerB
	g := Gate{Path: writeList(t, &File{Version: Version, Rules: []Rule{r}}), LocalWorker: func() (string, error) { return workerB, nil }}
	for _, name := range []string{"2024-11", "5-667"} {
		in := json.RawMessage(`{"target":{"session_name":"` + name + `"},"text":"x"}`)
		if _, err := g.Check(op(t, ops.OpSendText), in); err != nil {
			t.Errorf("local session %s: %v", name, err)
		}
	}
	g.LocalWorker = func() (string, error) { return workerA, nil } // rule: only "shell" of workerA
	in := json.RawMessage(`{"target":{"session_name":"2024-11"},"text":"x"}`)
	if _, err := g.Check(op(t, ops.OpSendText), in); !sameErr(err, ErrNotAllowed) {
		t.Fatalf("a local session outside the rule: %v", err)
	}
}

// A path that cannot hold a file (a directory of it is a file) is no
// allowlist; a file that exists but cannot be read still fails closed.
func TestGateTakesAPathThatCannotHoldAFileAsNoAllowlist(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := Gate{Path: filepath.Join(file, "minion", FileName)}
	in := json.RawMessage(`{"target":{"worker_id":"` + workerA + `","session_name":"x"}}`)
	if _, err := g.Check(op(t, ops.OpSessionSnapshot), in); err != nil {
		t.Fatalf("a read under ENOTDIR: %v", err)
	}
	if _, err := g.Check(op(t, ops.OpSendText), in); err != nil {
		t.Fatalf("a write under ENOTDIR without Required: %v", err)
	}
	g.Required = true
	if _, err := g.Check(op(t, ops.OpSendText), in); !sameErr(err, ErrNoRuleLeft) {
		t.Fatalf("a required write under ENOTDIR: %v", err)
	}
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("root (and Windows) reads a file of mode 0")
	}
	locked := writeList(t, &File{Version: Version, Rules: []Rule{sampleRule()}})
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	g = Gate{Path: locked}
	if _, err := g.Check(op(t, ops.OpSessionSnapshot), in); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("an unreadable allowlist passed a read it names: %v", err)
	}
}

func TestGateJudgesALocalTargetOnTheLocalWorker(t *testing.T) {
	r := sampleRule() // the whole of workerB
	path := writeList(t, &File{Version: Version, Rules: []Rule{r}})
	in := json.RawMessage(`{"target":{"session_name":"my-local"},"text":"x"}`)
	// The local socket runs a target without a worker_id on the local Worker
	// (workerA here), whatever the default Hub Worker says.
	local := Gate{Path: path, DefaultWorker: workerB, LocalWorker: func() (string, error) { return workerA, nil }}
	if _, err := local.Check(op(t, ops.OpSendText), in); !sameErr(err, ErrNotAllowed) {
		t.Fatalf("a local session under a rule of another Worker: %v", err)
	}
	local.LocalWorker = func() (string, error) { return workerB, nil }
	if _, err := local.Check(op(t, ops.OpSendText), in); err != nil {
		t.Fatalf("a local session of the rule's Worker: %v", err)
	}
	local.LocalWorker = func() (string, error) { return "", errors.New("no Worker") }
	if _, err := local.Check(op(t, ops.OpSendText), in); err == nil || !strings.Contains(err.Error(), "local Worker is not known") {
		t.Fatalf("an unknown local Worker: %v", err)
	}
	// A target that names its Worker needs no local id.
	named := json.RawMessage(`{"target":{"worker_id":"` + workerB + `","session_name":"x"},"text":"x"}`)
	if _, err := local.Check(op(t, ops.OpSendText), named); err != nil {
		t.Fatalf("a named Worker: %v", err)
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

// sameErr reports whether err is want itself: errors.Is matches every error
// of the same code.
func sameErr(err, want error) bool {
	return err != nil && err.Error() == want.Error()
}
