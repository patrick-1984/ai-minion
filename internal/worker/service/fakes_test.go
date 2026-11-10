package service

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// golden compares got with testdata/<name> (or rewrites it with -update).
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file; run: go test ./internal/worker/service -update\n--- got ---\n%s", name, got)
	}
}

// fakeRunner records every command and answers from a script.
type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	// answer returns the output and error for a command line; nil means
	// success with no output.
	answer func(line string) (string, error)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.mu.Lock()
	f.calls = append(f.calls, line)
	f.mu.Unlock()
	if f.answer == nil {
		return "", nil
	}
	return f.answer(line)
}

func (f *fakeRunner) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRunner) count(prefix string) int {
	n := 0
	for _, l := range f.lines() {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// testDeps returns Deps that write below a temp directory, never sleep and
// poll three times.
func testDeps(t *testing.T, r Runner) Deps {
	t.Helper()
	dir := t.TempDir()
	return Deps{
		Runner:           r,
		SystemUnitDir:    filepath.Join(dir, "etc-systemd-system"),
		PackagedUnitDirs: []string{filepath.Join(dir, "usr-lib-systemd-system")},
		Getenv:           func(k string) string { return map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}[k] },
		Sleep:            func(context.Context, time.Duration) {},
		ActivePolls:      3,
	}
}
