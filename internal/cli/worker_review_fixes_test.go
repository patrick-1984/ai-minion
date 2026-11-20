package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// Setup reloads, merges and saves the config file under the config lock of
// "worker config set", so a block saved meanwhile is not written over.
func TestUpStateUpdateHoldsTheConfigLock(t *testing.T) {
	dir := t.TempDir()
	release, err := lockConfigFile(filepath.Join(dir, configLockName))
	if err != nil {
		t.Fatal(err)
	}
	st := &upState{dir: dir}
	done := make(chan error, 1)
	go func() {
		_, err := st.update(func(f *config.ConfigFile) error { f.Set(config.KeyName, "w"); return nil })
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("setup saved while the config lock was held: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	// A "config set allow_new_sessions off" completes under the lock.
	f, _ := config.LoadFile(filepath.Join(dir, workerConfigName))
	f.Set(config.KeyAllowNewSessions, "off")
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ := config.LoadFile(filepath.Join(dir, workerConfigName))
	if v, _, _ := got.Lookup(config.KeyAllowNewSessions); v != "off" {
		t.Fatalf("setup erased the block: %q", v)
	}
	if v, _, _ := got.Lookup(config.KeyName); v != "w" {
		t.Fatalf("setup lost its own change: %q", v)
	}
}

// Without an explicit state directory the live Worker's profile is the one.
func TestPauseFollowsTheLiveWorkersProfileByDefault(t *testing.T) {
	r := &pauseRig{state: t.TempDir(), live: true}
	d := r.deps()
	d.call = func(context.Context, string, any) (json.RawMessage, error) {
		return json.Marshal(agent.LiveStatusReply{StateDir: "/some/other/profile"})
	}
	if live, err := d.liveFor(context.Background(), r.state, false); err != nil || !live {
		t.Fatalf("default: live=%v err=%v", live, err)
	}
	if live, _ := d.liveFor(context.Background(), r.state, true); live {
		t.Fatal("an explicit directory must not match another profile")
	}
}

// Setup saves allow_new_sessions only when this run gives it (flag or
// environment) and never overwrites a saved value otherwise.
func TestUpSavesAllowNewSessionsOnlyWhenGiven(t *testing.T) {
	f, _ := serviceGate(t, false, false, "build")
	if err := os.MkdirAll(f.stateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(f.stateDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(`{"allow_new_sessions":"off"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runIn(strings.NewReader(""), "--share-existing", "none"); err != nil {
		t.Fatalf("worker up: %v\n%s", err, f.stderr)
	}
	if got := readConfig(t, f.stateDir())["allow_new_sessions"]; got != "off" {
		t.Fatalf("a run that gave nothing rewrote the block: %q", got)
	}
	if err := os.WriteFile(cfg, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runIn(strings.NewReader(""), "--share-existing", "none", "--allow-new-sessions", "off"); err != nil {
		t.Fatalf("worker up: %v\n%s", err, f.stderr)
	}
	if got := readConfig(t, f.stateDir())["allow_new_sessions"]; got != "off" {
		t.Fatalf("an explicit --allow-new-sessions off was not saved: %q", got)
	}
}

// A paused Worker that is not enrolled refuses the join: no pretended success.
func TestJoinFromStdinRefusedWhilePaused(t *testing.T) {
	f, _ := serviceGate(t, false, false, "build")
	if err := agent.WritePause(f.stateDir(), true); err != nil {
		t.Fatal(err)
	}
	text, tok := testJoinToken(t, "https://hub.example:8443")
	_, err := stdinJoin(t, f, strings.NewReader(text+"\n"), "--share-existing", "none")
	if err == nil || !strings.Contains(err.Error(), "paused") || strings.Contains(err.Error(), tok.Invite.String()[10:]) {
		t.Fatalf("err = %v", err)
	}
	if len(f.runCalls()) != 0 {
		t.Fatalf("a service was installed: %v", f.runCalls())
	}
}
