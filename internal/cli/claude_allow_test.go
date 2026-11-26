package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/claudeallow"
)

const claudeTestWorker = "01890a5d-ac96-774b-bcce-b302099a8057"

type claudeTestEnv struct {
	dir, list string
	env       map[string]string
	stdin     string
	tty       bool
}

func newClaudeTestEnv(t *testing.T) *claudeTestEnv {
	t.Helper()
	root := t.TempDir()
	return &claudeTestEnv{dir: filepath.Join(root, "claude"), list: filepath.Join(root, "cfg", "minion", claudeallow.FileName), env: map[string]string{}}
}

func (e *claudeTestEnv) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	deps := claudeDeps{
		stdin:       strings.NewReader(e.stdin),
		lookupEnv:   func(k string) (string, bool) { v, ok := e.env[k]; return v, ok },
		interactive: func(io.Reader, io.Writer) bool { return e.tty },
		now:         func() time.Time { return time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC) },
		allowlist:   e.list,
	}
	cmd := newClaudeCmd(deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append(args, "--claude-config-dir", e.dir))
	err := cmd.Execute()
	return out.String(), err
}

func (e *claudeTestEnv) allow(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s.Permissions.Allow
}

func TestClaudeAllowStatusRevokeRoundTrip(t *testing.T) {
	e := newClaudeTestEnv(t)
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	prior := `{"theme":"dark","permissions":{"allow":["Bash(ls)","mcp__minion__session_snapshot"],"deny":["Bash(rm *)"]}}`
	if err := os.WriteFile(filepath.Join(e.dir, "settings.json"), []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := e.run(t, "allow", "--worker", claudeTestWorker, "--session", "shell", "--yes")
	if err != nil {
		t.Fatalf("allow: %v\n%s", err, out)
	}
	for _, want := range []string{`+ "mcp__minion__send_text"`, `= "mcp__minion__session_snapshot"`, claudeTestWorker + ": shell"} {
		if !strings.Contains(out, want) {
			t.Errorf("allow output lacks %q:\n%s", want, out)
		}
	}
	allow := e.allow(t)
	for _, want := range []string{"Bash(ls)", "mcp__minion__send_text", "mcp__minion__pipe_read", "mcp__minion__workers_list"} {
		if !slices.Contains(allow, want) {
			t.Errorf("settings lack %s: %v", want, allow)
		}
	}
	if slices.Contains(allow, "mcp__minion__invitation_create") || slices.Contains(allow, "mcp__minion__session_create") {
		t.Errorf("an admin or extra op was allowed: %v", allow)
	}
	raw, _ := os.ReadFile(filepath.Join(e.dir, "settings.json"))
	if !strings.Contains(string(raw), `"theme": "dark"`) || !strings.Contains(string(raw), `Bash(rm *)`) {
		t.Errorf("other settings were lost:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "settings.json.minion-backup")); err != nil {
		t.Errorf("no backup: %v", err)
	}
	list, err := claudeallow.Load(e.list)
	if err != nil || list == nil || len(list.Rules) != 1 {
		t.Fatalf("allowlist: %+v, %v", list, err)
	}
	id := list.Rules[0].ID

	// A second identical allow does not duplicate settings entries.
	if _, err := e.run(t, "allow", "--worker", claudeTestWorker, "--session", "shell", "--yes"); err != nil {
		t.Fatal(err)
	}
	if n := countEntry(e.allow(t), "mcp__minion__send_text"); n != 1 {
		t.Fatalf("send_text entries = %d", n)
	}

	out, err = e.run(t, "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var st claudeStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil || len(st.Rules) != 2 || len(st.Targets) != 2 {
		t.Fatalf("status: %v\n%s", err, out)
	}

	// Revoke one: the other still needs the entries.
	if _, err := e.run(t, "revoke", id); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.allow(t), "mcp__minion__send_text") {
		t.Fatal("an entry a remaining rule needs was removed")
	}
	if _, err := e.run(t, "revoke", "--all"); err != nil {
		t.Fatal(err)
	}
	allow = e.allow(t)
	if slices.Contains(allow, "mcp__minion__send_text") {
		t.Fatalf("revoke left the entry: %v", allow)
	}
	if !slices.Contains(allow, "mcp__minion__session_snapshot") || !slices.Contains(allow, "Bash(ls)") {
		t.Fatalf("revoke removed an entry the owner had before: %v", allow)
	}
	if _, err := os.Stat(e.list); !os.IsNotExist(err) {
		t.Fatalf("the empty allowlist was kept: %v", err)
	}
	out, err = e.run(t, "status")
	if err != nil || !strings.Contains(out, "No standing allow rule") || !strings.Contains(out, "mcp__minion__session_snapshot") {
		t.Fatalf("status after revoke: %v\n%s", err, out)
	}
}

func countEntry(list []string, e string) int {
	n := 0
	for _, x := range list {
		if x == e {
			n++
		}
	}
	return n
}

func TestClaudeAllowRefusesInsideClaudeCode(t *testing.T) {
	e := newClaudeTestEnv(t)
	e.env["CLAUDECODE"] = "1"
	if _, err := e.run(t, "allow", "--worker", claudeTestWorker, "--yes"); err == nil || !strings.Contains(err.Error(), "inside a Claude Code session") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "settings.json")); !os.IsNotExist(err) {
		t.Fatal("settings were written")
	}
}

func TestClaudeAllowAsksAndWritesNothingWithoutYes(t *testing.T) {
	e := newClaudeTestEnv(t)
	if _, err := e.run(t, "allow", "--worker", claudeTestWorker); err == nil {
		t.Fatal("no tty and no --yes wrote a rule")
	}
	e.tty, e.stdin = true, "n\n"
	out, err := e.run(t, "allow", "--worker", claudeTestWorker)
	if err != nil || !strings.Contains(out, "Nothing was written") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(e.list); !os.IsNotExist(err) {
		t.Fatal("the allowlist was written after no")
	}
	e.stdin = "y\n"
	if _, err := e.run(t, "allow", "--worker", claudeTestWorker); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.allow(t), "mcp__minion__send_keys") {
		t.Fatal("yes did not write")
	}
}

func TestClaudeAllowValidatesOpsAndTargets(t *testing.T) {
	e := newClaudeTestEnv(t)
	for _, args := range [][]string{
		{"allow", "--yes"},
		{"allow", "--worker", "laptop", "--yes"},
		{"allow", "--worker", claudeTestWorker, "--ops", "invitation_create", "--yes"},
		{"allow", "--worker", claudeTestWorker, "--session", "bad:name", "--yes"},
	} {
		if _, err := e.run(t, args...); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
	out, err := e.run(t, "allow", "--worker", claudeTestWorker, "--ops", "set_task,send_text", "--yes")
	if err != nil || !strings.Contains(out, "warning: set_task") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestClaudeAllowAddsThePluginServerWhenInstalled(t *testing.T) {
	e := newClaudeTestEnv(t)
	if err := os.MkdirAll(filepath.Join(e.dir, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "plugins", "installed_plugins.json"),
		[]byte(`{"version":2,"plugins":{"minion-control@patrick-1984":[{"scope":"user"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run(t, "allow", "--worker", claudeTestWorker, "--yes"); err != nil {
		t.Fatal(err)
	}
	allow := e.allow(t)
	if !slices.Contains(allow, "mcp__plugin_minion-control_minion__send_text") || !slices.Contains(allow, "mcp__minion__send_text") {
		t.Fatalf("allow = %v", allow)
	}
}
