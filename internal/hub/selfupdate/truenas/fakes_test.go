//go:build linux

package truenas

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/version"
)

const (
	prevID = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	candID = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	prevDg = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	candDg = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	repo   = "ghcr.io/example/minion"
)

type killed struct{ at int }

// world is one fake TrueNAS host: middleware, docker, zfs and the Hub.
type world struct {
	now             time.Time
	state           string
	compose         Compose
	data            string
	snaps           map[string]string
	holds           map[string]bool
	images          map[string]string
	running         bool
	runImg          string
	jobs            map[int]*fakeJob
	nextJob         int
	starts          int
	readonly        bool
	holdJobs        bool // new jobs stay RUNNING until released
	stuckDeploying  bool // a running candidate keeps the app DEPLOYING
	datasetName     string
	onEffect        func(name string)
	onVerified      func()
	blockContainers bool
	readyCalls      int

	log        []string
	violations []string
	points     int
	killAt     int

	candidateBroken bool
	rollbackFails   bool
	free            uint64
	releases        map[string]Release
}

func newWorld() *world {
	w := &world{
		now:   time.Date(2026, 11, 22, 3, 30, 0, 0, time.UTC),
		state: AppRunning, data: "v1", running: true, runImg: prevID,
		snaps: map[string]string{}, holds: map[string]bool{}, jobs: map[int]*fakeJob{}, datasetName: "tank/apps/hub", free: 100 << 30,
		images: map[string]string{
			repo + ":v0.16.0":   prevID,
			repo + "@" + prevDg: prevID,
			repo + "@" + candDg: candID,
		},
		compose: Compose{"services": map[string]any{"minion-hub": map[string]any{
			"image": repo + ":v0.16.0", "environment": map[string]any{"TOKEN": "s3cret-value"}}}},
	}
	w.releases = map[string]Release{
		"v0.16.0": {Tag: "v0.16.0", Repo: repo, Digest: prevDg, Range: pr("1.0..1.1")},
		"v0.17.0": {Tag: "v0.17.0", Repo: repo, Digest: candDg, Range: pr("1.0..1.2")},
		"latest":  {Tag: "v0.17.0", Repo: repo, Digest: candDg, Range: pr("1.0..1.2")},
	}
	return w
}

func pr(s string) version.ProtoRange {
	r, err := version.ParseProtoRange(s)
	if err != nil {
		panic(err)
	}
	return r
}

// point is a place where a run can be killed: before and after each effect.
func (w *world) point() {
	w.points++
	if w.killAt != 0 && w.points == w.killAt {
		panic(killed{w.points})
	}
}

func (w *world) effect(name string, f func()) {
	w.log = append(w.log, name)
	w.point()
	f()
	if w.onEffect != nil {
		w.onEffect(name)
	}
	w.point()
}

type fakeJob struct {
	method string
	state  JobState
}

func (w *world) newJob(method string) int {
	w.nextJob++
	st := JobSuccess
	if w.holdJobs {
		st = JobRunning
	}
	w.jobs[w.nextJob] = &fakeJob{method: method, state: st}
	return w.nextJob
}

func (w *world) releaseJobs() {
	for _, j := range w.jobs {
		if j.state == JobRunning {
			j.state = JobSuccess
		}
	}
}

// Middleware
func (w *world) AppState(context.Context, string) (string, error) {
	if w.stuckDeploying && w.running && w.runImg == candID && w.candidateBroken {
		return AppDeploying, nil
	}
	return w.state, nil
}
func (w *world) IsCustom(context.Context, string) (bool, error) { return true, nil }
func (w *world) FindJobs(_ context.Context, method, _ string, _ time.Time) ([]JobInfo, error) {
	var out []JobInfo
	for id, j := range w.jobs {
		if j.method == method {
			out = append(out, JobInfo{ID: id, State: j.state})
		}
	}
	return out, nil
}
func (w *world) AppConfig(context.Context, string) (Compose, error) {
	c, _ := w.compose.WithImage("minion-hub", w.mustImage())
	return c, nil
}
func (w *world) mustImage() string { i, _ := w.compose.Image("minion-hub"); return i }
func (w *world) Stop(context.Context, string) (int, error) {
	var id int
	w.effect("stop", func() { w.state, w.running = AppStopped, false; id = w.newJob("app.stop") })
	return id, nil
}
func (w *world) Start(context.Context, string) (int, error) {
	var id int
	w.effect("start", func() {
		w.boot()
		id = w.newJob("app.start")
	})
	return id, nil
}
func (w *world) boot() {
	w.state, w.running = AppRunning, true
	w.starts++
	w.runImg = w.images[w.mustImage()]
	w.readonly = w.compose.Env("minion-hub", MaintenanceEnv) == MaintenanceValue
	if w.runImg == candID {
		w.data = "migrated"
	}
}

func (w *world) Update(_ context.Context, _ string, c Compose) (int, error) {
	var id int
	img, _ := c.Image("minion-hub")
	label := "update:" + img
	if c.Env("minion-hub", MaintenanceEnv) != "" {
		label += "+ro"
	}
	w.effect(label, func() {
		redeploy := false
		if w.state != AppStopped {
			// Only lifting the read-only flag of the candidate may touch a running app.
			if w.compose.Env("minion-hub", MaintenanceEnv) != "" && c.Env("minion-hub", MaintenanceEnv) == "" {
				redeploy = true
			} else {
				w.violations = append(w.violations, "update while not stopped")
			}
		}
		w.compose = c
		if redeploy {
			w.boot()
		}
		id = w.newJob("app.update")
	})
	return id, nil
}
func (w *world) JobState(_ context.Context, id int) (JobState, error) {
	if j, ok := w.jobs[id]; ok {
		return j.state, nil
	}
	return JobUnknown, nil
}

// Docker
func (w *world) Pull(context.Context, string) error { w.log = append(w.log, "pull"); return nil }
func (w *world) ImageID(_ context.Context, ref string) (string, error) {
	if id, ok := w.images[ref]; ok {
		return id, nil
	}
	return "", ErrNoImage
}
func (w *world) Containers(ctx context.Context, _ string) ([]Container, error) {
	if w.blockContainers {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if !w.running {
		return nil, nil
	}
	return []Container{{ID: "c1", Service: "minion-hub", ImageID: w.runImg, Running: true,
		StartedAt: fmt.Sprintf("start-%d", w.starts), IP: "10.0.0.2"}}, nil
}
func (w *world) MountSource(context.Context, string, string) (string, error) {
	return "/mnt/tank/apps/hub", nil
}
func (w *world) DatasetAt(context.Context, string) (DatasetInfo, error) {
	return DatasetInfo{Name: w.datasetName, GUID: "4242"}, nil
}

// ZFS
func (w *world) Available(context.Context, string) (uint64, error) { return w.free, nil }
func (w *world) Snapshot(_ context.Context, _, n string) error {
	w.effect("snapshot", func() {
		if w.running {
			w.violations = append(w.violations, "snapshot while running")
		}
		w.snaps[n] = w.data
	})
	return nil
}
func (w *world) Hold(_ context.Context, _, n string) error {
	w.effect("hold", func() { w.holds[n] = true })
	return nil
}
func (w *world) Release(_ context.Context, _, n string) error {
	w.effect("release", func() { delete(w.holds, n) })
	return nil
}
func (w *world) SnapshotExists(_ context.Context, _, n string) (bool, error) {
	_, ok := w.snaps[n]
	return ok, nil
}
func (w *world) Rollback(_ context.Context, _, n string) error {
	var err error
	w.effect("rollback", func() {
		if w.running {
			w.violations = append(w.violations, "rollback while running")
		}
		if w.rollbackFails {
			err = errors.New("newer snapshot exists")
			return
		}
		w.data = w.snaps[n]
	})
	return err
}
func (w *world) Destroy(_ context.Context, _, n string) error {
	w.effect("destroy", func() {
		if w.holds[n] {
			w.violations = append(w.violations, "destroy of a held snapshot")
		}
		delete(w.snaps, n)
	})
	return nil
}

// Prober: the Hub is ready when it runs. The candidate image can be broken,
// and the previous image refuses the migrated database.
func (w *world) Ready(_ context.Context, url string) error {
	if url != "https://10.0.0.2:8443/readyz" {
		return errors.New("not this container")
	}
	w.readyCalls++
	if !w.running {
		return errors.New("down")
	}
	if w.runImg == candID && w.candidateBroken {
		return errors.New("broken")
	}
	if w.runImg == prevID && w.data == "migrated" {
		return errors.New("schema too new")
	}
	return nil
}

// Releases
func (w *world) Verified(_ context.Context, sel string) (Release, error) {
	if w.onVerified != nil {
		w.onVerified()
	}
	r, ok := w.releases[sel]
	if !ok {
		return Release{}, fmt.Errorf("no release %s", sel)
	}
	return r, nil
}

func (w *world) sleep(_ context.Context, d time.Duration) error { w.now = w.now.Add(d); return nil }

func newExec(t *testing.T, w *world) *Executor {
	t.Helper()
	dir := t.TempDir()
	j, err := OpenJournal(filepath.Join(dir, "state"), "ai-minion", -1, func() time.Time { return w.now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return &Executor{
		Cfg: Config{App: "ai-minion", Service: "minion-hub", Dataset: "tank/apps/hub",
			Window: "03-05", Channel: "latest", Enabled: true, MinFreeBytes: 1 << 20, DataMount: "/data",
			StatusFile: filepath.Join(dir, "out", "status.json"), RequestFile: filepath.Join(dir, "in", "request.json")},
		MW: w, Docker: w, ZFS: w, Releases: w, Probe: w, Journal: j, Timeouts: DefaultTimeouts(),
		Now: func() time.Time { return w.now }, Sleep: w.sleep,
	}
}

func (w *world) effects() string { return strings.Join(w.log, ",") }

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
