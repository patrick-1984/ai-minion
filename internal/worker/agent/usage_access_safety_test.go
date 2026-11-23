//go:build unix

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadProcessEnvKeepsNoSecretAndOnlyOwnProcesses(t *testing.T) {
	child := exec.Command("sleep", "30")
	child.Env = []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=sk-ant-TOPSECRET", "ANTHROPIC_BASE_URL=https://gw.example:8443/v1", "UNRELATED_TOKEN=leaky"}
	if err := child.Start(); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	// Start returns once exec has begun; the kernel publishes the new
	// program's environment a moment later, so wait for it.
	var (
		env []string
		ok  bool
	)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		env, _, ok = readProcessAccess(child.Process.Pid, false)
		if ok && len(env) > 0 || time.Now().After(deadline) {
			break
		}
	}
	if !ok {
		t.Fatal("own environment unreadable")
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "TOPSECRET") || strings.Contains(joined, "leaky") || strings.Contains(joined, "PATH=") {
		t.Fatalf("secret or unrelated entry kept: %q", env)
	}
	if envLookup(env, "ANTHROPIC_API_KEY") != "1" || envLookup(env, "ANTHROPIC_BASE_URL") != "custom" {
		t.Fatalf("presence/endpoint lost: %q", env)
	}
	old := workerUID
	t.Cleanup(func() { workerUID = old })
	workerUID = func() int { return os.Getuid() + 1 } // the process now belongs to someone else
	if _, _, ok := readProcessAccess(child.Process.Pid, false); ok {
		t.Fatal("read the environment of a process of another user")
	}
	if _, _, ok := readProcessAccess(0, false); ok {
		t.Fatal("pid 0 read")
	}
}

func TestEndpointIsReducedToVendorOrCustom(t *testing.T) {
	for raw, want := range map[string]string{
		"https://api.anthropic.com":        "vendor",
		"https://API.anthropic.com:443/v1": "vendor",
		"https://gw.example:8443/v1/x":     "custom",
		"https://sk-ant-api03-ABCDEF.ex":   "custom",
		"not a url":                        "custom",
		"https://user:pass@gw.example":     "custom",
	} {
		if got := endpointKind(raw, "api.anthropic.com"); got != want {
			t.Errorf("%q: got %q want %q", raw, got, want)
		}
	}
	// The host never survives the environment filter.
	env := filterProcessEnv([]byte("ANTHROPIC_BASE_URL=https://secret-gw.example/x\x00PATH=/bin"))
	if len(env) != 1 || env[0] != "ANTHROPIC_BASE_URL=custom" {
		t.Fatalf("%q", env)
	}
}

func TestProfileFileNamedPipeDoesNotStall(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "settings.json"), 0o600); err != nil {
		t.Skip("no fifo:", err)
	}
	fakeEnv(t, home, map[int][]string{4: {"PATH=/bin"}})
	done := make(chan sessionAccess, 1)
	go func() { done <- detectSessionAccess("claude", 4) }()
	select {
	case got := <-done:
		if got.Mode != "unknown" {
			t.Fatalf("got %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a named pipe stalled the collector")
	}
}

func TestSessionWithoutVerifiedProcessHasUnknownAccess(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeEnv(t, home, map[int][]string{})
	if got := detectSessionAccess("claude", 0); got.Mode != "unknown" {
		t.Fatalf("profile default used for an unverified process: %+v", got)
	}
}

func TestSettingsKeysAreOnlyPresenceChecked(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"tok","ANTHROPIC_BASE_URL":"https://user:pw@gw.example"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeEnv(t, home, map[int][]string{4: {"PATH=/bin"}})
	if got := detectSessionAccess("claude", 4); got != (sessionAccess{"api", accessCustomEndpoint}) {
		t.Fatalf("got %+v", got)
	}
}

func TestClaudeSettingsArgumentFromVerifiedProcessAndExporterAncestor(t *testing.T) {
	oldRoot, oldUID := procRoot, workerUID
	t.Cleanup(func() { procRoot, workerUID = oldRoot, oldUID })
	procRoot = t.TempDir()
	cwd := t.TempDir()
	file := filepath.Join(cwd, "proxy-settings.json")
	makeProc := func(pid, parent int, argv ...string) string {
		t.Helper()
		dir := filepath.Join(procRoot, strconv.Itoa(pid))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string]string{
			"cmdline": strings.Join(argv, "\x00") + "\x00",
			"stat":    strconv.Itoa(pid) + " (a process) S " + strconv.Itoa(parent) + " 1 0 0 0",
			"environ": "PATH=/bin\x00",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	dir := makeProc(2, 1, "claude", "--settings", file)
	makeProc(3, 2, "sh", "-c", "minion worker config usage _claude-export")
	makeProc(4, 2, "claude", "--", "--settings", file)
	relative := makeProc(5, 1, "node", "/node_modules/@anthropic-ai/claude-code/cli.js", "--settings=proxy-settings.json")
	if err := os.Symlink(cwd, filepath.Join(relative, "cwd")); err != nil {
		t.Fatal(err)
	}
	if _, settings, ok := readProcessAccess(2, true); !ok || settings != file {
		t.Fatalf("Worker settings: %q, process readable: %t", settings, ok)
	}
	if got := claudeAncestorSettings(3); got != file {
		t.Fatalf("exporter settings: %q", got)
	}
	if got := claudeAncestorSettings(4); got != "" {
		t.Fatal("treated prompt arguments or an outer Claude process as settings")
	}
	if got := claudeAncestorSettings(5); got != file {
		t.Fatalf("relative settings from the Claude working directory: %q", got)
	}
	if err := os.Remove(filepath.Join(dir, "cmdline")); err != nil {
		t.Fatal(err)
	}
	if got := claudeAncestorSettings(3); got != "" {
		t.Fatal("reported a settings argument from an unreadable command line")
	}
	workerUID = func() int { return os.Getuid() + 1 }
	if _, _, ok := readProcessAccess(2, true); ok || claudeAncestorSettings(5) != "" {
		t.Fatal("read settings evidence from another user's process")
	}
}
