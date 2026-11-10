package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func writeCfg(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFileMissingIsEmpty(t *testing.T) {
	f, err := LoadFile(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.Lookup("server"); ok || len(f.Keys()) != 0 {
		t.Fatal("expected empty file")
	}
}

func TestLoadFileValues(t *testing.T) {
	p := writeCfg(t, `{"server":"wss://hub","retries":3,"verbose":true,"ratio":1.5}`, 0o600)
	f, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"server": "wss://hub", "retries": "3", "verbose": "true", "ratio": "1.5"} {
		got, ok, err := f.Lookup(k)
		if err != nil || !ok || got != want {
			t.Errorf("%s = %q (ok %v, err %v), want %q", k, got, ok, err, want)
		}
	}
	if f.Path() != p {
		t.Error("Path")
	}
}

func TestLoadFileErrors(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"not json", `{`, "invalid JSON"},
		{"array", `[1]`, "invalid JSON"},
		{"nested", `{"a":{"b":1}}`, "must be a string, number or boolean"},
		{"null", `{"a":null}`, "must be a string, number or boolean"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadFile(writeCfg(t, c.content, 0o600))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
	// An empty or whitespace-only file is an empty config.
	if f, err := LoadFile(writeCfg(t, " \n", 0o600)); err != nil || len(f.Keys()) != 0 {
		t.Fatalf("empty file: %v", err)
	}
	if _, err := LoadFile(t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory: %v", err)
	}
}

func TestLoadFileRejectsLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	p := writeCfg(t, `{"server":"x"}`, 0o644)
	_, err := LoadFile(p)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("err = %v", err)
	}
}

func TestSaveRoundTripAndMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "dir", "config.json")
	f, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	f.Set("server", "wss://hub")
	f.Set("invite_key", "mnk_secret")
	f.Set("gone", "x")
	f.Delete("gone")
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %o, want 600", info.Mode().Perm())
		}
		dinfo, _ := os.Stat(filepath.Dir(p))
		if dinfo.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %o, want 700", dinfo.Mode().Perm())
		}
	}
	g, err := LoadFile(p) // also proves the saved file passes the mode check
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(g.Keys(), ","); got != "invite_key,server" {
		t.Errorf("keys = %s", got)
	}
	if v, _, _ := g.Lookup("server"); v != "wss://hub" {
		t.Errorf("server = %q", v)
	}
	// No temp files are left behind.
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want 1", len(entries))
	}
}

func TestSaveOverwritesLooseFileWithMode600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	p := writeCfg(t, `{}`, 0o644)
	f := &ConfigFile{path: p, vals: map[string]string{"a": "b"}}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestUnknownKeys(t *testing.T) {
	reg := NewRegistry()
	reg.Declare(Setting{Key: "server", Scope: ScopeWorker})
	reg.Declare(Setting{Key: "base_url", Scope: ScopeHub})
	f := &ConfigFile{vals: map[string]string{"server": "a", "base_url": "b", "typo": "c"}}
	got := f.UnknownKeys(reg, ScopeWorker)
	if strings.Join(got, ",") != "base_url,typo" {
		t.Fatalf("unknown = %v", got)
	}
}

func TestFileAsWorkerSource(t *testing.T) {
	p := writeCfg(t, `{"server":"wss://file"}`, 0o600)
	f, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Registry: workerRegistry(), Role: ScopeWorker, LookupEnv: envMap(nil), File: f}
	got, err := r.Resolve("server")
	if err != nil || got.Value != "wss://file" || got.Source != SourceFile {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestSaveConcurrentKeepsLatestSnapshot(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	f, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.Set("v", fmt.Sprint(i))
			if err := f.Save(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	mem, _, _ := f.Lookup("v")
	// Every Set precedes its own Save's snapshot, and saves are serialized
	// from snapshot to rename, so the file must hold the final memory state.
	g, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := g.Lookup("v"); got != mem {
		t.Errorf("persisted %q, memory %q", got, mem)
	}
}

func TestSaveRejectsOversizedFileAndKeepsOld(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	f, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	f.Set("k", "small")
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	f.Set("big", strings.Repeat("x", maxFileSize))
	if err := f.Save(); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized save: %v", err)
	}
	g, err := LoadFile(p)
	if err != nil {
		t.Fatalf("existing file no longer loads: %v", err)
	}
	if v, _, _ := g.Lookup("k"); v != "small" || len(g.Keys()) != 1 {
		t.Errorf("existing file changed: %v", g.Keys())
	}
	// A file just under the limit round-trips.
	f.Set("big", strings.Repeat("x", maxFileSize-200))
	if err := f.Save(); err != nil {
		t.Fatalf("near-limit save: %v", err)
	}
	if _, err := LoadFile(p); err != nil {
		t.Fatalf("near-limit file does not reload: %v", err)
	}
}
