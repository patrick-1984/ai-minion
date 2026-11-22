//go:build unix

// Package docsexamples runs the code examples of docs/guides/integrate-local.md
// against fixtures, so that the documented shell one-liner, the tmux progress
// bar and the Python reader keep working with the real output of the CLI.
package docsexamples

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const guide = "../../docs/guides/integrate-local.md"

// Block returns the first fenced block of the given language that follows the
// heading line starting with heading.
func Block(t testing.TB, heading, lang string) string {
	t.Helper()
	b, err := os.ReadFile(guide)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, heading)
	if i < 0 {
		t.Fatalf("heading %q not found", heading)
	}
	s = s[i:]
	open := "```" + lang + "\n"
	j := strings.Index(s, open)
	if j < 0 {
		t.Fatalf("no %s block after %q", lang, heading)
	}
	s = s[j+len(open):]
	k := strings.Index(s, "\n```")
	if k < 0 {
		t.Fatalf("unterminated block after %q", heading)
	}
	return s[:k+1]
}

func need(t *testing.T, tool string) {
	t.Helper()
	if _, err := exec.LookPath(tool); err != nil {
		t.Skipf("%s is not installed", tool)
	}
}

func fixture(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../../docs/schemas/fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// fakeMinion installs a "minion" that prints a discovery document in the shape
// of "minion worker discover --json [--usage]".
func fakeMinion(t *testing.T, workers []any) string {
	t.Helper()
	dir := t.TempDir()
	doc, _ := json.Marshal(map[string]any{"schema": "minion.worker-discovery/1", "workers": workers})
	if err := os.WriteFile(filepath.Join(dir, "doc.json"), doc, 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"$1 $2\" = \"worker discover\" ] || exit 64\ncat \"" + filepath.Join(dir, "doc.json") + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "minion"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runSh(t *testing.T, path, script string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=" + path + ":" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %s", err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

func running(t *testing.T) (map[string]any, int64) {
	now := time.Now().UnixMilli()
	usage := fixture(t, "usage_meters_full.json")
	for _, sl := range usage["slots"].([]any) {
		for _, m := range sl.(map[string]any)["meters"].([]any) {
			m.(map[string]any)["expires_at"] = float64(now + 600000)
		}
	}
	return map[string]any{"instance": "6f1c2f0e-3b7a-4c55-9d1e-0a6c1f7e2b10", "status": "running", "source": "private",
		"daemon_version": "v0.19.0", "os_scope": "user", "stale_after_s": 90, "usage": usage}, now
}

func TestShellOneLiner(t *testing.T) {
	need(t, "jq")
	script := Block(t, "### Shell: is it running", "sh")
	w, _ := running(t)
	if got := runSh(t, fakeMinion(t, []any{w}), script); got != "minion v0.19.0 running (user)" {
		t.Fatalf("running: %q", got)
	}
	if got := runSh(t, fakeMinion(t, []any{}), script); got != "minion not running" {
		t.Fatalf("none: %q", got)
	}
	stale := map[string]any{"status": "stale", "source": "private"}
	if got := runSh(t, fakeMinion(t, []any{stale}), script); got != "minion not running" {
		t.Fatalf("stale: %q", got)
	}
}

func TestTmuxProgressBar(t *testing.T) {
	need(t, "jq")
	bar := Block(t, "### tmux or status line", "sh")
	dir := fakeMinion(t, nil)
	w, _ := running(t)
	b, _ := json.Marshal(map[string]any{"schema": "minion.worker-discovery/1", "workers": []any{w}})
	if err := os.WriteFile(filepath.Join(dir, "doc.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "minion-bar"), []byte(bar), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := runSh(t, dir, "minion-bar"); got != "Claude Max [######----] 62%" {
		t.Fatalf("bar: %q", got)
	}
	// Expired meters and a label with control characters.
	for _, sl := range w["usage"].(map[string]any)["slots"].([]any) {
		for _, m := range sl.(map[string]any)["meters"].([]any) {
			m.(map[string]any)["expires_at"] = float64(1)
		}
	}
	b, _ = json.Marshal(map[string]any{"schema": "minion.worker-discovery/1", "workers": []any{w}})
	_ = os.WriteFile(filepath.Join(dir, "doc.json"), b, 0o600)
	if got := runSh(t, dir, "minion-bar"); got != "" {
		t.Fatalf("expired meter drawn: %q", got)
	}
	w, _ = running(t)
	w["usage"].(map[string]any)["slots"].([]any)[0].(map[string]any)["slot"].(map[string]any)["label"] = "Evil\x1b[31m"
	b, _ = json.Marshal(map[string]any{"schema": "minion.worker-discovery/1", "workers": []any{w}})
	_ = os.WriteFile(filepath.Join(dir, "doc.json"), b, 0o600)
	if got := runSh(t, dir, "minion-bar"); strings.ContainsRune(got, 0x1b) || !strings.HasPrefix(got, "Evil") {
		t.Fatalf("control character kept: %q", got)
	}
}

// WritePresence stores a record like a user Worker does: directory 0700, file 0600.
func WritePresence(t testing.TB, state string, rec map[string]any) {
	t.Helper()
	dir := filepath.Join(state, "minion", "presence")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(dir, rec["instance"].(string)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// RunPython runs the guide's Python example for the state home.
func RunPython(t testing.TB, state string) string {
	t.Helper()
	py := filepath.Join(t.TempDir(), "discover.py")
	if err := os.WriteFile(py, []byte(Block(t, "### Python", "python")), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-I", py)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(state, "nohome"), "XDG_STATE_HOME=" + state}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

var pythonOK = regexp.MustCompile(`^(running|stale|stopped) v0\.19\.0 \(user\)$`)

func TestPythonReader(t *testing.T) {
	need(t, "python3")
	rec := fixture(t, "presence_user_running.json")
	now := time.Now().UnixMilli()
	rec["started_at"], rec["updated_at"] = float64(now-60000), float64(now-1000)
	state := t.TempDir()
	WritePresence(t, state, rec)
	if got := RunPython(t, state); got != "running v0.19.0 (user)" {
		t.Fatalf("fresh: %q", got)
	}
	rec["updated_at"] = float64(now - 120000)
	WritePresence(t, state, rec)
	if got := RunPython(t, state); got != "stale v0.19.0 (user)" {
		t.Fatalf("stale: %q", got)
	}
	rec["updated_at"], rec["state"] = float64(now), "stopped"
	WritePresence(t, state, rec)
	if got := RunPython(t, state); !pythonOK.MatchString(got) || !strings.HasPrefix(got, "stopped") {
		t.Fatalf("stopped: %q", got)
	}
	if got := RunPython(t, t.TempDir()); got != "unavailable" {
		t.Fatalf("none: %q", got)
	}
}
