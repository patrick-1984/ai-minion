package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// share_lock is a real setting of `minion worker run`: the flag, the
// environment and the config file reach the settings every module gets, and the
// core module puts it in the policy (see the agent tests).
func TestWorkerRunShareLockReachesTheSettings(t *testing.T) {
	state := t.TempDir()
	file := filepath.Join(state, workerConfigName)
	if err := os.WriteFile(file, []byte(`{"share_lock":"true"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(env map[string]string, args ...string) bool {
		t.Helper()
		deps, p := probeOnlyDeps(t, env)
		cancel, wait := startWorker(t, deps, p, args...)
		locked := p.seen().ShareLock
		cancel()
		_ = wait()
		return locked
	}
	if !run(map[string]string{"MINION_STATE_DIR": state}) {
		t.Error("the config file did not lock")
	}
	if run(map[string]string{"MINION_STATE_DIR": state, "MINION_SHARE_LOCK": "false"}) {
		t.Error("the environment did not win over the file")
	}
	if !run(map[string]string{"MINION_STATE_DIR": state, "MINION_SHARE_LOCK": "false"}, "--share-lock") {
		t.Error("--share-lock (a switch, no value) did not win over the environment")
	}
	if run(map[string]string{"MINION_STATE_DIR": state}, "--share-lock=false") {
		t.Error("--share-lock=false did not lift it")
	}
	if run(map[string]string{"MINION_STATE_DIR": t.TempDir()}) {
		t.Error("the lock is on without being asked for")
	}
}

func TestWorkerRunRefusesAnInvalidShareLock(t *testing.T) {
	deps, p := probeOnlyDeps(t, map[string]string{})
	root := workerRoot(deps)
	root.SetArgs([]string{"worker", "run", "--share-lock=maybe"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.ExecuteContext(context.Background()); err == nil {
		t.Fatal("accepted")
	}
	if got := p.log(); len(got) != 0 {
		t.Fatalf("a module started although the setting is invalid: %v", got)
	}
}
