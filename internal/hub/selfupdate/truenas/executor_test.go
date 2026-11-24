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
	want := "pull,stop,snapshot,hold,update:" + repo + "@" + candDg + "+ro,start,update:" + repo + "@" + candDg + ",release,destroy"
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
	if _, err := e.Journal.LoadCompose(""); err == nil {
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

func TestWritableCandidateNeverRollsBackAcknowledgedData(t *testing.T) {
	w := newWorld()
	e := newExec(t, w)
	w.onEffect = func(name string) {
		if name == "update:"+repo+"@"+candDg {
			// A Worker reconnects after writers are enabled and its durable
			// turn is acknowledged. A later readiness failure must not erase it.
			w.data = "acknowledged-turn"
			w.candidateBroken = true
		}
	}
	if res := tick(t, e); res.Tick != TickPending {
		t.Fatalf("expected forward recovery, got %+v", res)
	}
	a, _ := e.Journal.LoadAttempt()
	if !a.KeepCandidate || a.Phase != PhaseFinalizing || w.data != "acknowledged-turn" || strings.Contains(w.effects(), "rollback") {
		t.Fatalf("lost forward decision or data: %+v, %s, %s", a, w.data, w.effects())
	}
	w.candidateBroken = false
	if res := tick(t, e); res.Tick != TickReconciled || w.data != "acknowledged-turn" {
		t.Fatalf("did not finish forward: %+v, %s", res, w.data)
	}
}

func TestTrialWithMinimalEnvironmentStillRollsBack(t *testing.T) {
	w := newWorld()
	svc, _ := w.compose.service("minion-hub")
	// Keep only the explicit data binding required for safe checkpoints.
	svc["environment"] = map[string]any{"MINION_DATA_DIR": "/data"}
	w.candidateBroken = true
	e := newExec(t, w)
	if res := tick(t, e); res.Tick != TickRolledBack || w.runImg != prevID || w.data != "v1" {
		t.Fatalf("failed to recognize own injected trial: %+v %s", res, w.effects())
	}
}

func TestUpdaterRefusesAValidButWrongDataMount(t *testing.T) {
	for _, scenario := range []string{"live-other-mount", "missing-explicit-dir", "stored-interpolation"} {
		t.Run(scenario, func(t *testing.T) {
			w := newWorld()
			e := newExec(t, w)
			svc, _ := w.compose.service("minion-hub")
			switch scenario {
			case "live-other-mount":
				w.dataDirOverride = "/real-data"
			case "missing-explicit-dir":
				delete(svc, "environment")
			case "stored-interpolation":
				svc["environment"] = map[string]any{"MINION_DATA_DIR": "${DATA_DIR}"}
				w.dataDirOverride = "/data"
			}
			res := tick(t, e)
			if res.Tick != TickRefused || res.Detail != "hub_data_mount_mismatch" || strings.Contains(w.effects(), "stop") || len(w.snaps) != 0 {
				t.Fatalf("wrong mount admitted: %+v effects=%s", res, w.effects())
			}
		})
	}
}

func TestFinalizationPreservesAdministratorComposeEdit(t *testing.T) {
	w := newWorld()
	e := newExec(t, w)
	w.onEffect = func(name string) {
		if name == "start" && w.runImg == candID {
			svc, _ := w.compose.service("minion-hub")
			svc["volumes"] = []any{"/operator/new-data:/data"}
		}
	}
	res := tick(t, e)
	if res.Tick != TickRecoveryRequired || res.Attempt.Reason != "compose_changed_by_admin" {
		t.Fatalf("operator edit was not preserved: %+v %s", res, w.effects())
	}
	svc, _ := w.compose.service("minion-hub")
	if svc["volumes"] == nil || strings.Contains(w.effects(), "rollback") {
		t.Fatal("operator document overwritten")
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
	want := "pull,stop,snapshot,hold,update:" + repo + "@" + candDg + "+ro,start," +
		"stop,rollback,update:" + repo + "@" + prevDg + ",start,release,destroy"
	if got := w.effects(); got != want {
		t.Fatalf("effects\n got %s\nwant %s", got, want)
	}
	noViolations(t, w)
	if w.data != "v1" || w.runImg != prevID {
		t.Fatalf("data %q image %q", w.data, w.runImg)
	}
	// The restore pins the verified previous DIGEST, never the mutable tag.
	if img, _ := w.compose.Image("minion-hub"); img != repo+"@"+prevDg {
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
	// Resolve is allowed only after the operator actually restored the
	// previous image; acknowledging a still-stopped app must fail.
	if err := e.ResolveRecovery(); err == nil {
		t.Fatal("resolved recovery without a verified restored Hub")
	}
	w.compose, _ = e.Journal.LoadCompose(a.SavedHash)
	w.data = "v1"
	w.boot()
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
		if a.Pending != nil {
			// A kill before a submission reply can leave no discoverable
			// job. Do not guess that it was never accepted: the checkpoint
			// and intent must stay intact with no further destructive work.
			before := w.effects()
			res := tick(t, e)
			if res.Tick != TickPending || w.effects() != before {
				t.Fatalf("ambiguous submission performed effects: %+v %s", res, w.effects())
			}
			return
		}
		t.Fatalf("kill %d/%d: attempt still at %s", k, j, a.Phase)
	}
	if w.state != AppRunning || !w.running || w.Ready(context.Background(), "https://10.0.0.2:8443/readyz") != nil {
		t.Fatalf("kill %d/%d: app not healthy: state %s image %s data %s (attempt %+v)", k, j, w.state, w.runImg, w.data, a)
	}
	img := w.mustImage()
	switch w.runImg {
	case prevID:
		if w.data != "v1" || (img != repo+":v0.16.0" && img != repo+"@"+prevDg) {
			t.Fatalf("kill %d/%d: previous image with data %q compose %q", k, j, w.data, img)
		}
	case candID:
		if w.data != "migrated" || img != repo+"@"+candDg || a == nil || a.Phase != PhaseCommitted || w.readonly {
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

func TestRunningTrialIsReadOnlyAndLiftedBeforeCommit(t *testing.T) {
	w := newWorld()
	e := newExec(t, w)
	sawRO := false
	w.onEffect = func(name string) {
		if name == "start" && w.readonly {
			sawRO = true
		}
	}
	if res := tick(t, e); res.Tick != TickUpdated {
		t.Fatalf("%+v", res)
	}
	if !sawRO || w.readonly || w.compose.Env("minion-hub", MaintenanceEnv) != "" {
		t.Fatalf("trial readonly=%v, final readonly=%v env=%q", sawRO, w.readonly, w.compose.Env("minion-hub", MaintenanceEnv))
	}
}

// H3: a job accepted by the middleware whose id was lost with the run keeps
// the next ticks pending; nothing is restored underneath it.
func TestAcceptedJobWithLostIDBlocksRecoveryUntilItEnds(t *testing.T) {
	w := newWorld()
	e := newExec(t, w)
	w.holdJobs = true
	w.killAt = 2 // after the stop effect, before its id is recorded
	if !runKilled(func() { _, _ = e.Tick(context.Background()) }) {
		t.Fatal("expected a kill")
	}
	w.killAt = 0
	w.now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	w.log = nil
	res := tick(t, e)
	if res.Tick != TickPending || len(w.log) != 0 {
		t.Fatalf("tick %+v effects %s", res, w.effects())
	}
	a, _ := e.Journal.LoadAttempt()
	if a.Phase == PhaseAborted || Terminal(a.Phase) {
		t.Fatalf("attempt ended while a job may run: %s", a.Phase)
	}
	w.releaseJobs()
	w.holdJobs = false
	for i := 0; i < 3; i++ {
		tick(t, e)
	}
	a, _ = e.Journal.LoadAttempt()
	if !Terminal(a.Phase) || !w.running || w.runImg != prevID || w.data != "v1" {
		t.Fatalf("phase %s running %v image %s", a.Phase, w.running, w.runImg)
	}
	noViolations(t, w)
}

// H3: a start job that times out is not raced by an immediate restore.
func TestJobTimeoutDoesNotRestoreWhileJobRuns(t *testing.T) {
	w := newWorld()
	e := newExec(t, w)
	e.Timeouts.Job = 30 * time.Second
	e.Timeouts.Reconcil = 30 * time.Second
	w.onEffect = func(name string) {
		if name == "start" {
			w.holdJobs = true // the job created next (none) ... keep the start job running
			for _, j := range w.jobs {
				if j.method == "app.start" {
					j.state = JobRunning
				}
			}
		}
	}
	res := tick(t, e)
	if res.Tick != TickPending {
		t.Fatalf("%+v", res)
	}
	if strings.Contains(w.effects(), "rollback") {
		t.Fatalf("restored while the start job runs: %s", w.effects())
	}
	w.onEffect, w.holdJobs = nil, false
	w.releaseJobs()
	w.candidateBroken = true
	for i := 0; i < 3; i++ {
		tick(t, e)
	}
	a, _ := e.Journal.LoadAttempt()
	if a.Phase != PhaseRolledBack || w.data != "v1" {
		t.Fatalf("phase %s data %s", a.Phase, w.data)
	}
}

// H4: DEPLOYING that never clears must not keep recovery pending.
func TestStuckDeployingStillRollsBack(t *testing.T) {
	w := newWorld()
	w.candidateBroken, w.stuckDeploying = true, true
	e := newExec(t, w)
	w.onEffect = func(name string) {
		if name == "start" {
			panic(killed{}) // the run dies right after starting the candidate
		}
	}
	if !runKilled(func() { _, _ = e.Tick(context.Background()) }) {
		t.Fatal("expected a kill")
	}
	w.onEffect = nil
	w.now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		tick(t, e)
	}
	a, _ := e.Journal.LoadAttempt()
	if a.Phase != PhaseRolledBack || w.runImg != prevID || w.data != "v1" {
		t.Fatalf("phase %s image %s data %s", a.Phase, w.runImg, w.data)
	}
}

// H5: the configured dataset must be the source of the Hub's data mount.
func TestWrongDatasetIsRefused(t *testing.T) {
	w := newWorld()
	w.datasetName = "tank/other"
	e := newExec(t, w)
	res := tick(t, e)
	if res.Tick != TickRefused || res.Detail != "dataset_mismatch" || strings.Contains(w.effects(), "stop") {
		t.Fatalf("%+v effects %s", res, w.effects())
	}
}

// H6: an administrator's edit is preserved, not overwritten.
func TestAdminEditIsNeverOverwritten(t *testing.T) {
	w := newWorld()
	e := newExec(t, w)
	admin, _ := w.compose.WithImage("minion-hub", repo+":v0.16.0")
	admin["x-admin"] = "edit"
	w.onEffect = func(name string) {
		if name == "stop" {
			w.compose = admin
		}
	}
	res := tick(t, e)
	if res.Tick != TickRecoveryRequired {
		t.Fatalf("%+v", res)
	}
	a, _ := e.Journal.LoadAttempt()
	if a.Reason != "compose_changed_by_admin" || w.compose["x-admin"] != "edit" {
		t.Fatalf("reason %s compose %v", a.Reason, w.compose)
	}
	if strings.Contains(w.effects(), "update") || strings.Contains(w.effects(), "rollback") {
		t.Fatalf("mutated: %s", w.effects())
	}
}

// H7: the restore refuses a previous image whose id changed.
func TestRestoreRefusesChangedPreviousImage(t *testing.T) {
	w := newWorld()
	w.candidateBroken = true
	e := newExec(t, w)
	w.onEffect = func(name string) {
		if name == "start" {
			w.images[repo+"@"+prevDg] = "sha256:" + strings.Repeat("7", 64)
		}
	}
	res := tick(t, e)
	a, _ := e.Journal.LoadAttempt()
	if res.Tick != TickRecoveryRequired || a.Reason != "previous_image_changed" {
		t.Fatalf("%+v %+v", res, a)
	}
}

// H8: readiness is asked of the container's own address and restarts the
// sustained timer when the container is replaced.
func TestSustainedReadinessRestartsOnContainerReplacement(t *testing.T) {
	w := newWorld()
	e := newExec(t, w)
	calls := 0
	w.onEffect = nil
	orig := w.starts
	e.Probe = probeFunc(func(ctx context.Context, url string) error {
		calls++
		if calls == 5 { // the container restarts mid-trial
			w.starts++
		}
		return w.Ready(ctx, url)
	})
	t0 := w.now
	if res := tick(t, e); res.Tick != TickUpdated {
		t.Fatalf("%+v", res)
	}
	if w.starts == orig || w.now.Sub(t0) < 60*time.Second+4*e.Timeouts.Poll {
		t.Fatalf("timer was not restarted: elapsed %s", w.now.Sub(t0))
	}
}

type probeFunc func(context.Context, string) error

func (f probeFunc) Ready(ctx context.Context, url string) error { return f(ctx, url) }

// M10: the installed tag follows from the single commit record.
func TestLastTagIsRepairedFromTheCommittedAttempt(t *testing.T) {
	w := newWorld()
	e := newExec(t, w)
	tick(t, e)
	if err := e.Journal.SaveState(State{}); err != nil { // as if the run died after the commit record
		t.Fatal(err)
	}
	tick(t, e)
	st, _ := e.Journal.LoadState()
	if st.LastTag != "v0.17.0" {
		t.Fatalf("last tag %q", st.LastTag)
	}
}

// H13: a stalled docker call cannot hold the run (and the lock) forever.
func TestStalledDockerCallIsBounded(t *testing.T) {
	w := newWorld()
	w.blockContainers = true
	e := newExec(t, w)
	e.Timeouts.Call = 50 * time.Millisecond
	done := make(chan TickResult, 1)
	go func() { r, _ := e.Tick(context.Background()); done <- r }()
	select {
	case r := <-done:
		if r.Tick != TickRefused {
			t.Fatalf("%+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled docker call held the run")
	}
}

// M14: the window and the one-shot are checked again right before the stop.
func TestWindowIsRecheckedBeforeDisruption(t *testing.T) {
	w := newWorld()
	w.now = time.Date(2026, 11, 22, 4, 55, 0, 0, time.UTC)
	e := newExec(t, w)
	w.onVerified = func() { w.now = time.Date(2026, 11, 22, 5, 10, 0, 0, time.UTC) } // slow verification
	res := tick(t, e)
	if res.Tick != TickRefused || res.Detail != "admission_changed" || strings.Contains(w.effects(), "stop") {
		t.Fatalf("%+v effects %s", res, w.effects())
	}
}

func TestWithdrawnOneShotIsNotRun(t *testing.T) {
	w := newWorld()
	e := newExec(t, w)
	e.Cfg.Enabled = false
	exp := w.now.Add(time.Hour).Format(time.RFC3339)
	writeFile(t, e.Cfg.RequestFile, `{"version":1,"once":{"id":"owner-0003","baseline":"v0.16.0","target":"v0.17.0","expires_at":"`+exp+`"}}`)
	w.onVerified = func() { _ = os.Remove(e.Cfg.RequestFile) }
	res := tick(t, e)
	if res.Tick != TickRefused || res.Detail != "admission_changed" || strings.Contains(w.effects(), "stop") {
		t.Fatalf("%+v effects %s", res, w.effects())
	}
}

// MINION_LISTEN as an unquoted number of the Compose environment map arrives
// from app.config as a JSON number, and an older padded form such as
// ":0009443" binds 9443: the updater probes the port the Hub listens on.
func TestReadinessProbesTheConfiguredListenPort(t *testing.T) {
	for _, v := range []any{float64(9443), "9443", ":0009443", "0.0.0.0:9443", "127.0.0.1:+9443", "0.0.0.0:00000000000000000009443"} {
		w := newWorld()
		w.readyPort = "9443"
		svc, _ := w.compose.service("minion-hub")
		svc["environment"].(map[string]any)["MINION_LISTEN"] = v
		e := newExec(t, w)
		if res := tick(t, e); res.Tick != TickUpdated {
			t.Fatalf("MINION_LISTEN %#v: tick = %+v (%s)", v, res, w.effects())
		}
	}
}

// An explicit listen port 0 binds an ephemeral port: the updater cannot probe
// it and never probes the default port in its place, so it declines before it
// changes anything (round 4). An absent MINION_LISTEN still means 8443.
func TestEphemeralListenPortIsRefused(t *testing.T) {
	for _, v := range []any{float64(0), "0", ":0", "0.0.0.0:0"} {
		w := newWorld()
		svc, _ := w.compose.service("minion-hub")
		svc["environment"].(map[string]any)["MINION_LISTEN"] = v
		e := newExec(t, w)
		if res := tick(t, e); res.Tick != TickRefused || res.Detail != "listen_port_ephemeral" || strings.Contains(w.effects(), "stop") {
			t.Fatalf("MINION_LISTEN %#v: tick = %+v (%s)", v, res, w.effects())
		}
	}
	w := newWorld()
	svc, _ := w.compose.service("minion-hub")
	delete(svc["environment"].(map[string]any), "MINION_LISTEN")
	if res := tick(t, newExec(t, w)); res.Tick != TickUpdated {
		t.Fatalf("no MINION_LISTEN: tick = %+v (%s)", res, w.effects())
	}
}

func TestComposeEnvReadsNumbers(t *testing.T) {
	c := Compose{"services": map[string]any{"hub": map[string]any{"environment": map[string]any{
		"A": float64(9443), "B": "x", "C": 1.5, "D": true, "E": nil}}}}
	for k, want := range map[string]string{"A": "9443", "B": "x", "C": "1.5", "D": "true", "E": "", "F": ""} {
		if got := c.Env("hub", k); got != want {
			t.Errorf("Env(%s) = %q, want %q", k, got, want)
		}
	}
}
