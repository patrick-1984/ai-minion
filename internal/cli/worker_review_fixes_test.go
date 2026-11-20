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

// Pause with no Worker answering writes nothing while a Worker holds the
// state (it is starting, or runs under another socket): no success.
func TestPauseOfflineRefusedWhileAWorkerHoldsTheState(t *testing.T) {
	dir := t.TempDir()
	l, err := agent.LockLifecycle(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePauseOffline(dir, true); err == nil || !strings.Contains(err.Error(), "starting or running") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, agent.PauseFile)); err == nil {
		t.Fatal("the pause file was written under a running Worker")
	}
	_ = l.Close()
	if err := writePauseOffline(dir, true); err != nil {
		t.Fatal(err)
	}
	if on, _ := agent.ReadPause(dir); !on {
		t.Fatal("not saved")
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

// A checked edit that is refused leaves the config file and the database as
// they were, and never touches the file before the revision check passed.
func TestStaleShareEditNeverWritesTheConfigFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"share_existing":"none"}`), 0o600); err != nil {
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
	cd.openStore = func(ctx context.Context, d string) (shareStore, error) {
		st, err := store.OpenContext(ctx, d)
		if err != nil {
			return nil, err
		}
		return fileCheckStore{st, t, dir}, nil
	}
	if _, _, err := configCmd(t, cd, dir, "share", "all", "--revision", rev); err == nil {
		t.Fatal("a stale edit was accepted")
	}
	if got := readConfig(t, dir)["share_existing"]; got != "none" {
		t.Fatalf("share_existing = %q after a refused edit", got)
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

// fileCheckStore asserts that the config file still holds the old value when
// the database transaction runs.
type fileCheckStore struct {
	*store.Store
	t   *testing.T
	dir string
}

func (s fileCheckStore) CommitShareChange(ctx context.Context, ch store.ShareChange) (store.ShareState, error) {
	if got := readConfig(s.t, s.dir)["share_existing"]; got != "none" {
		s.t.Errorf("the config file held %q before the revision check", got)
	}
	return s.Store.CommitShareChange(ctx, ch)
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

// A join token keeps the rule of the database: an older share_existing of the
// config file (the Hub narrowed the rule since) must not come back at restart.
func TestJoinFromStdinDoesNotRestoreAWiderShareExisting(t *testing.T) {
	f, _ := serviceGate(t, false, false, "build")
	if err := os.MkdirAll(f.stateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.stateDir(), "config.json"), []byte(`{"share_existing":"all"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(f.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CommitShareChange(context.Background(), store.ShareChange{Mode: store.ShareSelect, Names: []string{"build"}}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	text, _ := testJoinToken(t, "https://hub.example:8443")
	if _, err := stdinJoin(t, f, strings.NewReader(text+"\n")); err != nil {
		t.Fatalf("worker up: %v\n%s", err, f.stderr)
	}
	if v, ok := readConfig(t, f.stateDir())["share_existing"]; ok {
		t.Fatalf("share_existing = %q stayed in the config file", v)
	}
	if s := shareOf(t, f.stateDir()); s.Mode != store.ShareSelect {
		t.Fatalf("share = %+v", s)
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
