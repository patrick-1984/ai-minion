package config

import (
	"errors"
	"path/filepath"
	"testing"
)

// A failing directory sync fails Save, so a block is never acknowledged
// before it is durable.
func TestSaveSyncsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	f, err := LoadFile(filepath.Join(dir, "worker.json"))
	if err != nil {
		t.Fatal(err)
	}
	var synced string
	old := syncDirFn
	defer func() { syncDirFn = old }()
	syncDirFn = func(d string) error { synced = d; return nil }
	f.Set("allow_new_sessions", "off")
	if err := f.Save(); err != nil || synced != dir {
		t.Fatalf("save: %v, synced %q", err, synced)
	}
	syncDirFn = func(string) error { return errors.New("eio") }
	if err := f.Save(); err == nil {
		t.Fatal("a failed directory sync was ignored")
	}
}
