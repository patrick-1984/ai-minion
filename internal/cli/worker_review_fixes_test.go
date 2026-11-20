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
	"github.com/patrick-1984/ai-minion/internal/worker/store"
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

// Every share edit, a refused one included, removes the share_existing
// override from the config file, so that no restart applies an older, wider
// file value over the database; a refused edit leaves the database as it was.
func TestShareEditLeavesNoStartupOverrideBehind(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"share_existing":"all"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cd := testConfigDeps(nil)
	if _, _, err := configCmd(t, cd, dir, "share", "none"); err != nil {
		t.Fatal(err)
	}
	rev := shareOfRevision(t, dir)
	if _, _, err := configCmd(t, cd, dir, "share", "none"); err != nil { // another change
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"share_existing":"all"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := configCmd(t, cd, dir, "share", "all", "--revision", rev); err == nil {
		t.Fatal("a stale edit was accepted")
	}
	if v, ok := readConfig(t, dir)["share_existing"]; ok {
		t.Fatalf("share_existing = %q stayed in the file", v)
	}
	if s := loadShare(t, dir); s.Mode != store.ShareNone {
		t.Fatalf("share = %+v", s)
	}
}

func shareOfRevision(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(strings.Split(strings.Split(shareListText(t, dir), "(revision ")[1], ")")[0])
}

func shareListText(t *testing.T, dir string) string {
	t.Helper()
	out, _, err := configCmd(t, testConfigDeps(nil), dir, "share", "list")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The inventory is offered only for a host that is certainly the Worker's.
func TestShareListNeedsAKnownHost(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(map[string]string{"TMUX_TMPDIR": "/somewhere/else"})
	cd.sessions = func(string) agent.StatusSessions { return listerNames{"a"} }
	if r := shareList(t, cd, dir); r.SessionsKnown || len(r.Sessions) != 0 {
		t.Fatalf("a shell with its own TMUX_TMPDIR got an inventory: %+v", r)
	}
	// A socket saved in the config file is the service's too.
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"tmux_socket":"`+filepath.Join(dir, "t.sock")+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := shareList(t, cd, dir); !r.SessionsKnown {
		t.Fatalf("a configured socket was not trusted: %+v", r)
	}
	// A socket only this shell names (flag or environment) is not.
	cd = testConfigDeps(map[string]string{"MINION_TMUX_SOCKET": filepath.Join(dir, "x.sock")})
	cd.sessions = func(string) agent.StatusSessions { return listerNames{"a"} }
	_ = os.Remove(filepath.Join(dir, "config.json"))
	if r := shareList(t, cd, dir); r.SessionsKnown {
		t.Fatalf("a shell-only socket was trusted: %+v", r)
	}
}

// Setup never imports allow_new_sessions: an "on" in this shell does not lift
// a block saved in the config file.
func TestUpDoesNotTouchAllowNewSessions(t *testing.T) {
	f, _ := serviceGate(t, false, false, "build")
	if err := os.MkdirAll(f.stateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.stateDir(), "config.json"), []byte(`{"allow_new_sessions":"off"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f.env["MINION_ALLOW_NEW_SESSIONS"] = "on"
	if _, err := f.runIn(strings.NewReader(""), "--share-existing", "none", "--allow-new-sessions", "on"); err != nil {
		t.Fatalf("worker up: %v\n%s", err, f.stderr)
	}
	if got := readConfig(t, f.stateDir())["allow_new_sessions"]; got != "off" {
		t.Fatalf("setup rewrote the block: %q", got)
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
