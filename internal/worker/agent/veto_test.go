package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
)

// vetoRig is an update trial From -> To whose new binary keeps failing.
type vetoRig struct {
	dir, exe string
	log      *slog.Logger
	now      time.Time
}

func newVetoRig(t *testing.T, from, to string) *vetoRig {
	t.Helper()
	r := &vetoRig{dir: t.TempDir(), log: slog.New(slog.NewTextHandler(&logBuf{}, nil)), now: time.Now()}
	bin := t.TempDir()
	r.exe = filepath.Join(bin, "minion")
	if err := os.WriteFile(r.exe, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.exe+".old", []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writePending(r.dir, pendingRecord{From: from, To: to, At: r.now}); err != nil {
		t.Fatal(err)
	}
	return r
}

// failStarts runs guardMaxStarts+1 failing starts of to and reports whether
// any of them rolled back.
func (r *vetoRig) failStarts(to string) bool {
	for i := 0; i <= guardMaxStarts; i++ {
		if _, restart, _ := guardOnStart(r.log, r.dir, r.exe, to, r.now.Add(time.Duration(i)*time.Second)); restart {
			return true
		}
	}
	return false
}

func (r *vetoRig) kept(t *testing.T) {
	t.Helper()
	if b, _ := os.ReadFile(r.exe); string(b) != "new" {
		t.Fatalf("exe = %q, the rollback was not vetoed", b)
	}
	if _, ok := readPending(r.dir); !ok {
		t.Fatal("the guard record was removed; it must stay")
	}
}

// A pause acknowledged before the incoming update record existed still
// vetoes the rollback to a release that cannot pause.
func TestVetoRestrictionBeforeGuardExists(t *testing.T) {
	dir := t.TempDir()
	if _, err := SetPause(dir, true, time.Time{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	r := newVetoRig(t, "v0.14.0", "v0.15.1")
	if _, err := SetPause(r.dir, true, time.Time{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.failStarts("v0.15.1") {
		t.Fatal("rolled back to v0.14.0 while paused")
	}
	r.kept(t)
}

// A block saved after the trial began (after the migration) vetoes too; an
// unparsable From counts as older.
func TestVetoAfterMigration(t *testing.T) {
	for _, from := range []string{"v0.14.2", "garbage"} {
		r := newVetoRig(t, from, "v0.15.0")
		cfg, err := config.LoadFile(filepath.Join(r.dir, WorkerConfigFile))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Set(config.KeyAllowNewSessions, "off")
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
		if r.failStarts("v0.15.0") {
			t.Fatalf("from %s: rolled back under a block", from)
		}
		r.kept(t)
	}
}

// A trial from a release that enforces both keeps its crash protection.
func TestVetoDoesNotApplyToNewFrom(t *testing.T) {
	r := newVetoRig(t, "v0.15.0", "v0.15.1")
	if _, err := SetPause(r.dir, true, time.Time{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !r.failStarts("v0.15.1") {
		t.Fatal("no rollback to v0.15.0, which can pause")
	}
	// Without a restriction an older From rolls back as before.
	r2 := newVetoRig(t, "v0.14.0", "v0.15.1")
	if !r2.failStarts("v0.15.1") {
		t.Fatal("no rollback without a restriction")
	}
}

// A setter that cannot persist answers an error and changes nothing.
func TestSetterFailsOnGuardIO(t *testing.T) {
	saved := pauseFS
	t.Cleanup(func() { pauseFS = saved })
	pauseFS.sync = func(*os.File) error { return errors.New("I/O error") }
	svc, err := newPauseService(t.TempDir(), discardLogger(), time.Now, func(time.Duration, func()) func() bool { return func() bool { return true } })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Pause(context.Background(), time.Time{}); err == nil || svc.Paused() {
		t.Fatalf("pause = %v, paused %t", err, svc.Paused())
	}
}

// A restart before the first health tick keeps the restriction: it is read
// from the state files at the next start.
func TestRestartBeforeHealthTickKeepsRestriction(t *testing.T) {
	dir := t.TempDir()
	svc, err := NewPauseService(dir, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Pause(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	again, err := NewPauseService(dir, discardLogger()) // the next process
	if err != nil || !again.Paused() {
		t.Fatalf("after a restart: paused %t, %v", again.Paused(), err)
	}
}

// A paused connector counts as a trust hold: the trial is committed.
func TestPausedCommitsTrial(t *testing.T) {
	if h := connectorHealth(Status{State: ConnIdle, Reason: ReasonPaused}, ""); h != healthTrustHold {
		t.Fatalf("paused health = %v, want healthTrustHold", h)
	}
	if h := connectorHealth(Status{State: ConnBackoff}, ""); h == healthTrustHold {
		t.Fatal("backoff is not a trust hold")
	}
}

// After more failed starts than the guard allows, the vetoed binary is still
// the one that runs: it starts paused and blocked.
func TestVetoedBinaryStillEnforces(t *testing.T) {
	r := newVetoRig(t, "v0.14.0", "v0.15.0")
	if _, err := SetPause(r.dir, true, time.Time{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for k := 0; k < 3; k++ {
		if r.failStarts("v0.15.0") {
			t.Fatal("rolled back")
		}
	}
	svc, err := NewPauseService(r.dir, discardLogger())
	if err != nil || !svc.Paused() {
		t.Fatalf("not paused after the unclean starts: %v", err)
	}
}
