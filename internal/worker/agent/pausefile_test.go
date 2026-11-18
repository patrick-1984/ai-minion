package agent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Every change bumps the generation, resume included; a reread sees it.
func TestPauseFileGenerationMonotonic(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	if p, err := ReadPause(dir); err != nil || p.Paused || p.Generation != 0 {
		t.Fatalf("missing file = %+v, %v", p, err)
	}
	var last uint64
	for i, step := range []struct {
		paused bool
		until  time.Time
	}{
		{true, now.Add(30 * time.Minute)},
		{true, time.Time{}}, // a timed pause replaced by an indefinite one
		{false, time.Time{}},
		{false, time.Time{}}, // a repeated resume still counts
		{true, now.Add(time.Hour)},
	} {
		p, err := SetPause(dir, step.paused, step.until, now)
		if err != nil {
			t.Fatal(err)
		}
		if p.Generation <= last {
			t.Fatalf("step %d: generation %d after %d", i, p.Generation, last)
		}
		last = p.Generation
		got, err := ReadPause(dir)
		if err != nil || got.Generation != p.Generation || got.Paused != step.paused || !got.Until.Equal(p.Until) {
			t.Fatalf("step %d: reread %+v, %v; want %+v", i, got, err, p)
		}
	}
	got, _ := ReadPause(dir)
	if !got.Active(now) || got.Active(now.Add(2*time.Hour)) {
		t.Fatalf("Active: %+v", got)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, PauseFile))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("pause file mode = %v, %v; want 0600", fi.Mode(), err)
		}
	}
	if removed, err := ClearPause(dir); !removed || err != nil {
		t.Fatalf("ClearPause = %t, %v", removed, err)
	}
	if removed, err := ClearPause(dir); removed || err != nil {
		t.Fatalf("second ClearPause = %t, %v", removed, err)
	}
}

// A failed fsync, rename or directory flush is an error and leaves the old
// state in place: the change must not be acknowledged.
func TestPauseFileWriteErrorsAreReported(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func()
	}{
		{"fsync", func() { pauseFS.sync = func(*os.File) error { return errors.New("disk full") } }},
		{"rename", func() { pauseFS.rename = func(string, string) error { return errors.New("read-only") } }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := pauseFS
			t.Cleanup(func() { pauseFS = saved })
			dir := t.TempDir()
			if _, err := SetPause(dir, false, time.Time{}, time.Now()); err != nil {
				t.Fatal(err)
			}
			tc.inject()
			if _, err := SetPause(dir, true, time.Time{}, time.Now()); err == nil {
				t.Fatal("the failed write was reported as done")
			}
			if p, err := ReadPause(dir); err != nil || p.Paused || p.Generation != 1 {
				t.Fatalf("state after the failure = %+v, %v; want the old one", p, err)
			}
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if e.Name() != PauseFile {
					t.Fatalf("left behind: %s", e.Name())
				}
			}
		})
	}
}

func TestPauseFileInvalidIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PauseFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPause(dir); err == nil {
		t.Fatal("an invalid pause file was read as not paused")
	}
	if _, err := SetPause(dir, true, time.Time{}, time.Now()); err == nil {
		t.Fatal("an invalid pause file was overwritten")
	}
}
