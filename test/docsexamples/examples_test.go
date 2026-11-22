//go:build unix

// Package docsexamples runs the code examples of docs/guides/integrate-local.md
// against fixtures, so that the documented shell one-liner, the tmux progress
// bar and the Python reader keep working with the real output of the CLI.
package docsexamples

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
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
func fakeMinion(t *testing.T, workers []any) string { return fakeMinionExit(t, workers, 0) }

// fakeMinionExit is fakeMinion that exits with status code (the document is
// printed only when code is 0 or 1).
func fakeMinionExit(t *testing.T, workers []any, code int) string {
	t.Helper()
	dir := t.TempDir()
	doc, _ := json.Marshal(map[string]any{"schema": "minion.worker-discovery/1", "workers": workers})
	if err := os.WriteFile(filepath.Join(dir, "doc.json"), doc, 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"$1 $2\" = \"worker discover\" ] || exit 64\n[ " + strconv.Itoa(code) + " -gt 1 ] || cat \"" + filepath.Join(dir, "doc.json") + "\"\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "minion"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runSh(t *testing.T, path, script string) string {
	t.Helper()
	out, code := runShStatus(t, path, script)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	return out
}

// runShStatus runs script and returns its output and exit status.
func runShStatus(t *testing.T, path, script string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=" + path + ":" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return strings.TrimSpace(out.String() + errb.String()), ee.ExitCode()
		}
		t.Fatal(err)
	}
	return strings.TrimSpace(out.String()), 0
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
	// "none running" (exit 1) is an answer; a failed search (exit 2) keeps its
	// status and is not reported as "not running".
	if got, code := runShStatus(t, fakeMinionExit(t, []any{}, 1), script); code != 0 || got != "minion not running" {
		t.Fatalf("exit 1: %d %q", code, got)
	}
	if got, code := runShStatus(t, fakeMinionExit(t, nil, 2), script); code != 2 || !strings.Contains(got, "search failed") {
		t.Fatalf("exit 2: %d %q", code, got)
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
	// The first slot's headline is expired, the second slot's is fresh: the bar
	// shows the second.
	w, now := running(t)
	slots := w["usage"].(map[string]any)["slots"].([]any)
	if len(slots) < 2 {
		t.Fatal("fixture needs two slots")
	}
	for _, m := range slots[0].(map[string]any)["meters"].([]any) {
		m.(map[string]any)["expires_at"] = float64(now - 1000)
	}
	second := slots[1].(map[string]any)
	m2 := second["meters"].([]any)[0].(map[string]any)
	second["headline"], m2["remaining_percent"] = m2["id"], float64(40)
	b, _ = json.Marshal(map[string]any{"schema": "minion.worker-discovery/1", "workers": []any{w}})
	_ = os.WriteFile(filepath.Join(dir, "doc.json"), b, 0o600)
	if got := runSh(t, dir, "minion-bar"); got != "Team builds [####------] 40%" {
		t.Fatalf("second slot not used: %q", got)
	}
	w, _ = running(t)
	w["usage"].(map[string]any)["slots"].([]any)[0].(map[string]any)["slot"].(map[string]any)["label"] = "Evil\x1b[31m"
	b, _ = json.Marshal(map[string]any{"schema": "minion.worker-discovery/1", "workers": []any{w}})
	_ = os.WriteFile(filepath.Join(dir, "doc.json"), b, 0o600)
	if got := runSh(t, dir, "minion-bar"); strings.ContainsRune(got, 0x1b) || !strings.HasPrefix(got, "Evil") {
		t.Fatalf("control character kept: %q", got)
	}
}

// presenceDir is where the guide's Python example looks for the account's
// records when HOME and XDG_STATE_HOME point into state: the XDG state home on
// Linux, Application Support on macOS.
func presenceDir(state string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(state, "Library", "Application Support", "minion", "presence")
	}
	return filepath.Join(state, "minion", "presence")
}

// WritePresence stores a record like a user Worker does: directory 0700, file 0600.
func WritePresence(t testing.TB, state string, rec map[string]any) {
	t.Helper()
	dir := presenceDir(state)
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
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + state, "XDG_STATE_HOME=" + state}
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

// A planted FIFO must not hang the scan, and a record with a null or mistyped
// field must not abort it: the fresh record next to them is still found.
func TestPythonReaderSurvivesHostileCandidates(t *testing.T) {
	need(t, "python3")
	rec := fixture(t, "presence_user_running.json")
	now := time.Now().UnixMilli()
	rec["started_at"], rec["updated_at"] = float64(now-60000), float64(now-1000)
	state := t.TempDir()
	WritePresence(t, state, rec)
	dir := filepath.Join(state, "minion", "presence")
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, mod := range []func(map[string]any){
		func(m map[string]any) { m["updated_at"] = nil },
		func(m map[string]any) { m["stale_after_s"] = "90" },
		func(m map[string]any) { m["started_at"] = true },
		func(m map[string]any) { delete(m, "state") },
	} {
		bad := fixture(t, "presence_user_running.json")
		bad["instance"] = "bad" + strconv.Itoa(i)
		bad["updated_at"] = float64(now)
		mod(bad)
		WritePresence(t, state, bad)
	}
	done := make(chan string, 1)
	go func() { done <- RunPython(t, state) }()
	select {
	case got := <-done:
		if got != "running v0.19.0 (user)" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the scan hangs on a FIFO")
	}
}
