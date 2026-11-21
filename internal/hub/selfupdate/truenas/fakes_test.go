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
	now     time.Time
	state   string
	compose Compose
	data    string
	snaps   map[string]string
	holds   map[string]bool
	images  map[string]string
	running bool
	runImg  string
	jobs    map[int]JobState
	nextJob int

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
		snaps: map[string]string{}, holds: map[string]bool{}, jobs: map[int]JobState{}, free: 100 << 30,
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
	w.point()
}

func (w *world) newJob() int {
	w.nextJob++
	w.jobs[w.nextJob] = JobSuccess
	return w.nextJob
}

// Middleware
func (w *world) AppState(context.Context, string) (string, error) { return w.state, nil }
func (w *world) AppConfig(context.Context, string) (Compose, error) {
	c, _ := w.compose.WithImage("minion-hub", w.mustImage())
	return c, nil
}
func (w *world) mustImage() string { i, _ := w.compose.Image("minion-hub"); return i }
func (w *world) Stop(context.Context, string) (int, error) {
	var id int
	w.effect("stop", func() { w.state, w.running = AppStopped, false; id = w.newJob() })
	return id, nil
}
func (w *world) Start(context.Context, string) (int, error) {
	var id int
	w.effect("start", func() {
		w.state, w.running = AppRunning, true
		w.runImg = w.images[w.mustImage()]
		if w.runImg == candID {
			w.data = "migrated"
		}
		id = w.newJob()
	})
	return id, nil
}
func (w *world) Update(_ context.Context, _ string, c Compose) (int, error) {
	var id int
	img, _ := c.Image("minion-hub")
	w.effect("update:"+img, func() {
		if w.state != AppStopped {
			w.violations = append(w.violations, "update while not stopped")
		}
		w.compose, _ = c.WithImage("minion-hub", img)
		id = w.newJob()
	})
	return id, nil
}
func (w *world) JobState(_ context.Context, id int) (JobState, error) {
	if s, ok := w.jobs[id]; ok {
		return s, nil
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
func (w *world) Containers(context.Context, string) ([]Container, error) {
	if !w.running {
		return nil, nil
	}
	return []Container{{ID: "c1", Service: "minion-hub", ImageID: w.runImg, Running: true}}, nil
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
func (w *world) Ready(context.Context) error {
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
	return &Executor{
		Cfg: Config{App: "ai-minion", Service: "minion-hub", Dataset: "tank/apps/hub", ReadinessURL: "http://127.0.0.1:1/readyz",
			Window: "03-05", Channel: "latest", Enabled: true, MinFreeBytes: 1 << 20,
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
