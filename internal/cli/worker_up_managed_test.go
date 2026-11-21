package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// fakeManagedTmux writes a tmux that answers -V and logs its arguments; no
// tmux is on PATH in these tests, so a probe of PATH would fail.
func fakeManagedTmux(t *testing.T) (bin, log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script tmux")
	}
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	bin = filepath.Join(dir, "libtmux.so")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\necho 'tmux 3.8'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	return bin, log
}

func managedEnv(f *upFixture, bin string) {
	f.env["MINION_MANAGED_HOST"] = "android"
	f.env["MINION_TMUX_BINARY"] = bin
	f.env["MINION_TMUX_SOCKET"] = filepath.Join(f.home, "run", "tmux.sock")
	delete(f.env, "MINION_HUB_FINGERPRINT")
}

// The provisioning command of the Android service works on a fresh state with
// pipes only: the first-run gate checks the bundled tmux (never PATH), the
// sharing choice is saved once, the token is read from stdin and the Hub and
// its pin are saved. The managed settings are never written to the config
// file: the path of the binary moves with every APK update.
func TestWorkerUpConfigureOnlyJoinOnAFreshManagedState(t *testing.T) {
	f, _ := serviceGate(t, false, false)
	bin, log := fakeManagedTmux(t)
	managedEnv(f, bin)
	f.deps.setup.checkTerminal = checkTerminalHost
	var seen joinSeen
	joinHook(t, f, &seen)
	text, _ := testJoinToken(t, "https://hub.example:8443")
	_, err := f.runIn(strings.NewReader(text+"\n"), "--configure-only", "--non-interactive-share", "all", "--join", "-")
	if err != nil {
		t.Fatalf("worker up: %v\nstderr: %s", err, f.stderr)
	}
	if seen.hub.Server != "https://hub.example:8443" {
		t.Errorf("the hook saw %+v", seen.hub)
	}
	if got := shareOf(t, f.stateDir()); !got.Configured || got.Mode != store.ShareAll {
		t.Errorf("share = %+v", got)
	}
	cfg := readConfig(t, f.stateDir())
	if cfg["server"] != "https://hub.example:8443" || cfg["hub_fingerprint"] != testHubFingerprint() {
		t.Errorf("config = %v", cfg)
	}
	for _, k := range []string{"tmux_binary", "managed_host", "tmux_config", "share_existing"} {
		if _, ok := cfg[k]; ok {
			t.Errorf("%s was saved: %v", k, cfg)
		}
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the bundled tmux was not run: %v", err)
	}
	if !strings.Contains(string(raw), "-N -f /dev/null") {
		t.Errorf("the preflight ran tmux without -N and the fixed configuration: %q", raw)
	}
	if len(f.runCalls()) != 0 {
		t.Errorf("a service was installed: %v", f.runCalls())
	}
}

// Without the managed binary the same gate would look for tmux on PATH (and
// fail here): the managed path is what makes the preflight work.
func TestWorkerUpGateWithoutTheManagedBinaryProbesPATH(t *testing.T) {
	f, _ := serviceGate(t, false, false)
	_, _ = fakeManagedTmux(t)
	f.deps.setup.checkTerminal = checkTerminalHost
	_, err := f.runIn(strings.NewReader(""), "--configure-only", "--non-interactive-share", "all")
	if err == nil || !strings.Contains(err.Error(), "tmux") {
		t.Fatalf("err = %v", err)
	}
}

// A join killed between the enrollment commit and the config save: the next
// "worker up" (without any token) finishes it from the committed enrollment,
// keeping the Worker key and never needing the used token again.
func TestWorkerUpRecoversAnInterruptedJoin(t *testing.T) {
	f, _ := serviceGate(t, false, false)
	bin, _ := fakeManagedTmux(t)
	managedEnv(f, bin)
	f.deps.setup.checkTerminal = checkTerminalHost
	hub, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var keyBefore []byte
	f.hook("enroll", func(s agent.Settings) error {
		st, err := store.Open(s.StateDir)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		e := agent.NewEnrollments(st, s.StateDir)
		if _, err := e.LoadOrCreateKey(); err != nil {
			return err
		}
		keyBefore, _ = os.ReadFile(e.KeyPath())
		if err := e.Commit(context.Background(), agent.Enrollment{
			Pin: identity.Pin{Pub: hub.Public()}, WorkerID: "w-1", HubURL: "https://hub.example:8443",
			HubCheck: store.HubCheckInvite, HubCheckAnchor: hub.Fingerprint().String(),
		}); err != nil {
			return err
		}
		return os.ErrDeadlineExceeded // the process died here
	})
	key, err := identity.NewInviteKey(hub.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	token, err := identity.NewJoinToken("https://hub.example:8443", key)
	if err != nil {
		t.Fatal(err)
	}
	text := token.Encode()
	if _, err := f.runIn(strings.NewReader(text+"\n"), "--configure-only", "--non-interactive-share", "all", "--join", "-"); err == nil {
		t.Fatal("the interrupted join reported success")
	}
	if cfg := readConfig(t, f.stateDir()); cfg["server"] != "" {
		t.Fatalf("the interrupted run saved the Hub: %v", cfg)
	}

	sd := testStatusDeps(map[string]string{"MINION_STATE_DIR": f.stateDir()})
	sd.loadConfig = loadWorkerConfig
	sd.live = func(string) agent.LiveSource {
		return fakeLive{ls: agent.LiveStatus{Connector: agent.Status{
			State: agent.ConnConnected, Welcome: agent.HubWelcome{Pending: true},
		}, ApprovalKnown: true}}
	}
	raw, _, err := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--json")
	var status agent.StatusReport
	if err != nil || json.Unmarshal([]byte(raw), &status) != nil ||
		status.Mode != agent.ModeHub || status.HubURL != "https://hub.example:8443" || status.Approval != agent.ApprovalPending {
		t.Fatalf("recovered status: %s; error %v", raw, err)
	}

	// The next start: no token, no server setting.
	f.registry = agent.NewRegistry()
	f.deps.run.registry = f.registry
	if _, err := f.runIn(strings.NewReader(""), "--configure-only", "--non-interactive-share", "all"); err != nil {
		t.Fatalf("recovery run: %v\nstderr: %s", err, f.stderr)
	}
	cfg := readConfig(t, f.stateDir())
	if cfg["server"] != "https://hub.example:8443" || cfg["hub_fingerprint"] != hub.Fingerprint().String() {
		t.Errorf("config after recovery = %v", cfg)
	}
	if after, _ := os.ReadFile(filepath.Join(f.stateDir(), agent.WorkerKeyFile)); string(after) != string(keyBefore) || len(after) == 0 {
		t.Error("the Worker key changed")
	}
}

func TestFillRecoveredHubKeepsWhatIsConfigured(t *testing.T) {
	dir := t.TempDir()
	file, err := config.LoadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	fillRecoveredHub(file, nil)
	if len(file.Keys()) != 0 {
		t.Fatalf("nil recovery wrote %v", file.Keys())
	}
	rec := &agent.RecoveredHub{URL: "https://recovered.example", Fingerprint: testHubFingerprint()}
	file.Set(config.KeyServer, "https://configured.example")
	fillRecoveredHub(file, rec)
	if v, _, _ := file.Lookup(config.KeyServer); v != "https://configured.example" {
		t.Errorf("server = %q", v)
	}
	if _, ok, _ := file.Lookup(config.KeyHubFingerprint); ok {
		t.Error("a fingerprint was added next to a configured Hub")
	}
	file.Delete(config.KeyServer)
	file.SetPendingJoin(rec.URL, rec.Fingerprint)
	fillRecoveredHub(file, rec)
	if v, _, _ := file.Lookup(config.KeyServer); v != "https://recovered.example" {
		t.Errorf("server = %q", v)
	}
}

// "worker status" counts the sessions with the managed binary and flags, never
// with tmux from PATH.
func TestWorkerStatusUsesTheManagedTmux(t *testing.T) {
	bin, log := fakeManagedTmux(t)
	dir := t.TempDir()
	env := map[string]string{"MINION_STATE_DIR": dir, "MINION_MANAGED_HOST": "android", "MINION_TMUX_BINARY": bin,
		"MINION_TMUX_SOCKET": filepath.Join(dir, "tmux.sock")}
	sd := testStatusDeps(env)
	sd.sessions = func(socket string) agent.StatusSessions { return tmuxSessions(socket) }
	out, _, err := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	raw, rerr := os.ReadFile(log)
	if rerr != nil || !strings.Contains(string(raw), "-N -f /dev/null") {
		t.Fatalf("the managed tmux was not used as managed: %q %v\n%s", raw, rerr, out)
	}
}
