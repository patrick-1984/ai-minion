// Package dircache seeds test directories from a snapshot that is built once
// per test binary. It is test support only: a test that needs a data directory
// with an already migrated database pays for the migration once instead of
// once per test (under the race detector a migration costs several hundred
// milliseconds). Nothing in the production code uses it.
package dircache

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type file struct {
	rel  string
	mode fs.FileMode
	data []byte
}

type entry struct {
	once  sync.Once
	files []file
	err   error
}

var (
	mu      sync.Mutex
	entries = map[string]*entry{}
)

// Seed fills dir with the snapshot named key. The first call for a key runs
// build in a scratch directory (the real, slow path) and keeps the resulting
// files in memory; later calls only write them. dir must not hold the files
// yet.
func Seed(t testing.TB, key, dir string, build func(dir string) error) {
	t.Helper()
	mu.Lock()
	e := entries[key]
	if e == nil {
		e = &entry{}
		entries[key] = e
	}
	mu.Unlock()
	e.once.Do(func() { e.files, e.err = snapshot(build) })
	if e.err != nil {
		t.Fatalf("dircache %s: %v", key, e.err)
	}
	for _, f := range e.files {
		p := filepath.Join(dir, f.rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.data, f.mode); err != nil {
			t.Fatal(err)
		}
	}
}

func snapshot(build func(dir string) error) ([]file, error) {
	scratch, err := os.MkdirTemp("", "dircache-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	if err := build(scratch); err != nil {
		return nil, err
	}
	var out []file
	err = filepath.WalkDir(scratch, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(scratch, p)
		out = append(out, file{rel: rel, mode: info.Mode().Perm(), data: data})
		return nil
	})
	return out, err
}
