//go:build linux

package truenas

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func tick(t *testing.T, e *Executor) TickResult {
	t.Helper()
	res, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	return res
}

func noViolations(t *testing.T, w *world) {
	t.Helper()
	if len(w.violations) > 0 {
		t.Fatalf("violations: %v", w.violations)
	}
}

func TestHappyPath(t *testing.T) {
	w := newWorld()
	e := newExec(t, w)
	res := tick(t, e)
	if res.Tick != TickUpdated {
		t.Fatalf("tick = %+v", res)
	}
	want := "pull,stop,snapshot,hold,update:" + repo + "@" + candDg + ",start,release,destroy"
	if got := w.effects(); got != want {
		t.Fatalf("effects\n got %s\nwant %s", got, want)
	}
	noViolations(t, w)
	if img, _ := w.compose.Image("minion-hub"); img != repo+"@"+candDg {
		t.Fatalf("stored image %q", img)
	}
	if w.data != "migrated" || w.runImg != candID {
		t.Fatalf("data %q image %q", w.data, w.runImg)
	}
	a, _ := e.Journal.LoadAttempt()
	st, _ := e.Journal.LoadState()
	if a.Phase != PhaseCommitted || st.LastTag != "v0.17.0" {
		t.Fatalf("attempt %+v state %+v", a, st)
	}
	if _, err := e.Journal.LoadCompose(); err == nil {
		t.Fatal("saved compose must be dropped after commit")
	}
	raw, err := os.ReadFile(e.Cfg.StatusFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "s3cret") || !strings.Contains(string(raw), `"tick": "updated"`) {
		t.Fatalf("status file: %s", raw)
	}
	// The next tick sees the release as current and does nothing.
	w.log = nil
	if res := tick(t, e); res.Tick != TickUpToDate || len(w.log) != 0 {
		t.Fatalf("second tick %+v effects %s", res, w.effects())
	}
}

func TestFailureAfterMigrationRestoresInOrder(t *testing.T) {
	w := newWorld()
	w.candidateBroken = true
	e := newExec(t, w)
	res := tick(t, e)
	if res.Tick != TickRolledBack {
		t.Fatalf("tick = %+v", res)
	}
	want := "pull,stop,snapshot,hold,update:" + repo + "@" + candDg + ",start," +
		"stop,rollback,update:" + repo + ":v0.16.0,start,release,destroy"
	if got := w.effects(); got != want {
		t.Fatalf("effects\n got %s\nwant %s", got, want)
	}
	noViolations(t, w)
	if w.data != "v1" || w.runImg != prevID {
		t.Fatalf("data %q image %q", w.data, w.runImg)
	}
	if img, _ := w.compose.Image("minion-hub"); img != repo+":v0.16.0" {
		t.Fatalf("stored image %q", img)
	}
	st, _ := e.Journal.LoadState()
	if len(st.Quarantine) != 1 || st.Quarantine[0] != candDg {
		t.Fatalf("quarantine %v", st.Quarantine)
	}
	// Quarantined: no retry, no side effects.
	w.log = nil
	res = tick(t, e)
	if res.Tick != TickRefused || res.Detail != "candidate_quarantined" || strings.Contains(w.effects(), "stop") {
		t.Fatalf("retry %+v effects %s", res, w.effects())
	}
	if err := e.ClearQuarantine(); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackFailureNeedsRecoveryAndKeepsCheckpoint(t *testing.T) {
	w := newWorld()
	w.candidateBroken, w.rollbackFails = true, true
	e := newExec(t, w)
	res := tick(t, e)
	if res.Tick != TickRecoveryRequired {
		t.Fatalf("tick = %+v", res)
	}
	a, _ := e.Journal.LoadAttempt()
	if a.Phase != PhaseRecoveryRequired || a.Reason != "rollback_failed" {
		t.Fatalf("attempt %+v", a)
	}
	if len(w.snaps) != 1 || len(w.holds) != 1 || w.state != AppStopped {
		t.Fatalf("checkpoint not preserved: snaps %v holds %v state %s", w.snaps, w.holds, w.state)
	}
	// Later ticks do nothing until the operator resolves it.
	w.log = nil
	if res := tick(t, e); res.Tick != TickRecoveryRequired || len(w.log) != 0 {
		t.Fatalf("second tick %+v effects %s", res, w.effects())
	}
	if err := e.ResolveRecovery(); err != nil {
		t.Fatal(err)
	}
}

// runKilled runs f and reports whether the simulated process was killed.
func runKilled(f func()) (k bool) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(killed); !ok {
				panic(r)
			}
			k = true
		}
	}()
	f()
	return false
}

func TestInterruptedRunsAreReconciledAtEveryStep(t *testing.T) {
	for _, broken := range []bool{false, true} {
		for k := 1; ; k++ {
			w := newWorld()
			w.candidateBroken = broken
			e := newExec(t, w)
			w.killAt = k
			if !runKilled(func() { _, _ = e.Tick(context.Background()) }) {
				break // the run completed: every point was tried
			}
			w.killAt = 0
			checkReconciled(t, w, e, k, -1)
		}
	}
}

func TestDoubleInterruptionIsReconciled(t *testing.T) {
	for k := 1; k <= 18; k += 2 {
		for j := 1; j <= 24; j += 3 {
			w := newWorld()
			w.candidateBroken = k%4 == 1
			e := newExec(t, w)
			w.killAt = k
			if !runKilled(func() { _, _ = e.Tick(context.Background()) }) {
				continue
			}
			// The second run is killed too, outside the window.
			w.now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
			w.points, w.killAt = 0, j
			_ = runKilled(func() { _, _ = e.Tick(context.Background()) })
			w.killAt = 0
			checkReconciled(t, w, e, k, j)
		}
	}
}

// checkReconciled runs a tick outside the window and checks the end state.
func checkReconciled(t *testing.T, w *world, e *Executor, k, j int) {
	t.Helper()
	w.now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC) // outside 03-05
	for i := 0; i < 3; i++ {
		_, err := e.Tick(context.Background())
		if err != nil {
			t.Fatalf("kill %d/%d: reconcile: %v", k, j, err)
		}
		a, _ := e.Journal.LoadAttempt()
		if a == nil || Terminal(a.Phase) {
			break
		}
	}
	noViolations(t, w)
	a, _ := e.Journal.LoadAttempt()
	if a != nil && !Terminal(a.Phase) {
		t.Fatalf("kill %d/%d: attempt still at %s", k, j, a.Phase)
	}
	if w.state != AppRunning || !w.running || w.Ready(context.Background()) != nil {
		t.Fatalf("kill %d/%d: app not healthy: state %s image %s data %s (attempt %+v)", k, j, w.state, w.runImg, w.data, a)
	}
	img := w.mustImage()
	switch w.runImg {
	case prevID:
		if w.data != "v1" || img != repo+":v0.16.0" {
			t.Fatalf("kill %d/%d: previous image with data %q compose %q", k, j, w.data, img)
		}
	case candID:
		if w.data != "migrated" || img != repo+"@"+candDg || a == nil || a.Phase != PhaseCommitted {
			t.Fatalf("kill %d/%d: candidate state data %q compose %q attempt %+v", k, j, w.data, img, a)
		}
	}
	if len(w.snaps) != 0 || len(w.holds) != 0 {
		t.Fatalf("kill %d/%d: leftover snapshots %v holds %v", k, j, w.snaps, w.holds)
	}
}

func TestOverlappingRunsDoNotRace(t *testing.T) {
	w := newWorld()
	e := newExec(t, w)
	unlock, err := e.Journal.Lock()
	if err != nil {
		t.Fatal(err)
	}
	res := tick(t, e)
	if res.Tick != TickBusy || len(w.log) != 0 {
		t.Fatalf("overlapping tick %+v effects %s", res, w.effects())
	}
	unlock()
	if res := tick(t, e); res.Tick != TickUpdated {
		t.Fatalf("after unlock %+v", res)
	}
}

func TestOutsideWindowOnlyReconciles(t *testing.T) {
	w := newWorld()
	e := newExec(t, w)
	w.now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	if res := tick(t, e); res.Tick != TickOutsideWindow || len(w.log) != 0 {
		t.Fatalf("%+v effects %s", res, w.effects())
	}
	e.Cfg.Enabled = false
	w.now = time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	if res := tick(t, e); res.Tick != TickDisabled || len(w.log) != 0 {
		t.Fatalf("%+v effects %s", res, w.effects())
	}
	// A leftover attempt is reconciled although the updater is disabled and
	// the Hub is down.
	e.Cfg.Enabled = true
	w.now = time.Date(2026, 11, 22, 3, 30, 0, 0, time.UTC)
	w.killAt = 6 // inside the start step, after the stop and the staging
	if !runKilled(func() { _, _ = e.Tick(context.Background()) }) {
		t.Fatal("expected a kill")
	}
	w.killAt = 0
	e.Cfg.Enabled = false
	w.now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	res := tick(t, e)
	if res.Tick != TickReconciled || !w.running || w.runImg != prevID && w.runImg != candID {
		t.Fatalf("reconcile %+v running %v image %s", res, w.running, w.runImg)
	}
}

func TestPolicyRefusals(t *testing.T) {
	cases := map[string]func(w *world){
		"protocol_incompatible": func(w *world) {
			r := w.releases["latest"]
			r.Range = pr("1.1..1.2") // does not contain the baseline range 1.0..1.1
			w.releases["latest"] = r
		},
		"candidate_not_stable": func(w *world) {
			r := w.releases["latest"]
			r.Prerelease = true
			w.releases["latest"] = r
		},
		"not_enough_space":       func(w *world) { w.free = 10 },
		"release_unverified":     func(w *world) { delete(w.releases, "latest") },
		"running_image_mismatch": func(w *world) { w.runImg = "sha256:" + strings.Repeat("9", 64) },
	}
	for want, mutate := range cases {
		t.Run(want, func(t *testing.T) {
			w := newWorld()
			mutate(w)
			e := newExec(t, w)
			res := tick(t, e)
			if res.Tick != TickRefused || res.Detail != want {
				t.Fatalf("%+v", res)
			}
			if strings.Contains(w.effects(), "stop") {
				t.Fatalf("disturbed the app: %s", w.effects())
			}
		})
	}
	t.Run("not newer", func(t *testing.T) {
		w := newWorld()
		w.releases["latest"] = w.releases["v0.16.0"]
		e := newExec(t, w)
		if res := tick(t, e); res.Tick != TickUpToDate {
			t.Fatalf("%+v", res)
		}
	})
}

func TestLegacyOneShotRequest(t *testing.T) {
	w := newWorld()
	e := newExec(t, w)
	e.Cfg.Enabled = false
	w.now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC) // outside the window
	exp := w.now.Add(time.Hour).Format(time.RFC3339)
	writeFile(t, e.Cfg.RequestFile, `{"version":1,"once":{"id":"owner-0001","baseline":"v0.16.0","target":"v0.17.0","expires_at":"`+exp+`"}}`)
	res := tick(t, e)
	if res.Tick != TickUpdated {
		t.Fatalf("%+v", res)
	}
	noViolations(t, w)
	// Consumed: a second tick with the same file does nothing.
	w.log = nil
	w.releases["latest"] = Release{Tag: "v0.18.0", Repo: repo, Digest: "sha256:" + strings.Repeat("c", 64), Range: pr("1.0..1.2")}
	if res := tick(t, e); res.Tick != TickDisabled || len(w.log) != 0 {
		t.Fatalf("second tick %+v effects %s", res, w.effects())
	}
	raw, _ := os.ReadFile(e.Cfg.StatusFile)
	if !strings.Contains(string(raw), "owner-0001") {
		t.Fatalf("status lacks the handled request: %s", raw)
	}
}

func TestOneShotWithWrongBaselineIsRefused(t *testing.T) {
	w := newWorld()
	e := newExec(t, w)
	exp := w.now.Add(time.Hour).Format(time.RFC3339)
	w.releases["v0.15.0"] = Release{Tag: "v0.15.0", Repo: repo, Digest: "sha256:" + strings.Repeat("d", 64), Range: pr("1.0..1.1")}
	writeFile(t, e.Cfg.RequestFile, `{"version":1,"once":{"id":"owner-0002","baseline":"v0.15.0","target":"v0.17.0","expires_at":"`+exp+`"}}`)
	res := tick(t, e)
	if res.Tick != TickRefused || strings.Contains(w.effects(), "stop") {
		t.Fatalf("%+v effects %s", res, w.effects())
	}
}

func TestBadRequestDoesNotBlockReconcileOrAttempts(t *testing.T) {
	w := newWorld()
	e := newExec(t, w)
	writeFile(t, e.Cfg.RequestFile, "{not json")
	res := tick(t, e)
	if res.Tick != TickUpdated {
		t.Fatalf("%+v", res)
	}
	raw, _ := os.ReadFile(e.Cfg.StatusFile)
	if !strings.Contains(string(raw), "request_invalid_json") {
		t.Fatalf("status lacks the request error: %s", raw)
	}
}

func TestHubDownIsNotTouched(t *testing.T) {
	w := newWorld()
	w.state, w.running = AppStopped, false
	e := newExec(t, w)
	if res := tick(t, e); res.Tick != TickHubDown || len(w.log) != 0 {
		t.Fatalf("%+v effects %s", res, w.effects())
	}
}
