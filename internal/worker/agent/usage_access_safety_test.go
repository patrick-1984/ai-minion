//go:build unix

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
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
	env, ok := readProcessEnv(child.Process.Pid)
	if !ok {
		t.Fatal("own environment unreadable")
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "TOPSECRET") || strings.Contains(joined, "leaky") || strings.Contains(joined, "PATH=") {
		t.Fatalf("secret or unrelated entry kept: %q", env)
	}
	if envLookup(env, "ANTHROPIC_API_KEY") != "1" || envLookup(env, "ANTHROPIC_BASE_URL") != "https://gw.example:8443/v1" {
		t.Fatalf("presence/endpoint lost: %q", env)
	}
	old := workerUID
	t.Cleanup(func() { workerUID = old })
	workerUID = func() int { return os.Getuid() + 1 } // the process now belongs to someone else
	if _, ok := readProcessEnv(child.Process.Pid); ok {
		t.Fatal("read the environment of a process of another user")
	}
	if _, ok := readProcessEnv(0); ok {
		t.Fatal("pid 0 read")
	}
}

func TestEndpointHostRefusesCredentialLookingHosts(t *testing.T) {
	long := strings.Repeat("a", 64) + ".example"
	for raw, want := range map[string]string{
		"https://gw.example:8443/v1/x":                     "gw.example:8443",
		"http://10.0.0.5:4000":                             "10.0.0.5:4000",
		"https://api.anthropic.com":                        "",
		"https://sk-ant-api03-ABCDEF.example":              "",
		"https://user:pass@gw.example":                     "",
		"https://sk-ant-secret@gw.example":                 "",
		"https://" + long:                                  "",
		"https://" + strings.Repeat("a.", 130) + "example": "",
		"https://gw_example":                               "",
		"https://-bad.example":                             "",
		"https://gw.example:99999":                         "",
		"sk-ant-secret":                                    "",
		"https://token-abc.example":                        "",
	} {
		if got := endpointHost(raw, "api.anthropic.com"); got != want {
			t.Errorf("%q: got %q want %q", raw, got, want)
		}
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
	if got := detectSessionAccess("claude", 4); got != (sessionAccess{"api", ""}) {
		t.Fatalf("got %+v", got)
	}
}
