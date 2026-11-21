//go:build linux

package truenas

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWindow(t *testing.T) {
	cases := []struct {
		w    string
		hour int
		in   bool
	}{{"03-05", 3, true}, {"03-05", 4, true}, {"03-05", 5, false}, {"03-05", 2, false},
		{"22-02", 23, true}, {"22-02", 1, true}, {"22-02", 2, false}, {"22-02", 12, false}, {"00-24", 13, true}}
	for _, c := range cases {
		w, err := ParseWindow(c.w)
		if err != nil {
			t.Fatal(err)
		}
		if got := w.Contains(time.Date(2026, 1, 1, c.hour, 30, 0, 0, time.UTC)); got != c.in {
			t.Errorf("%s at %d: %v", c.w, c.hour, got)
		}
	}
	for _, bad := range []string{"", "3-5", "03-03", "25-02", "03-25", "ab-cd", "03:05"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestConfigValidateAndLoad(t *testing.T) {
	c := Config{App: "ai-minion", Dataset: "zasob/docker/aiminion/data", StateDir: "/mnt/tank/u/state"}.WithDefaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Config{
		{App: "Bad App", Dataset: c.Dataset, StateDir: c.StateDir},
		{App: c.App, Dataset: "/mnt/zasob/data", StateDir: c.StateDir},
		{App: c.App, Dataset: "pool/../x", StateDir: c.StateDir},
		{App: c.App, Dataset: "pool", StateDir: c.StateDir},
		{App: c.App, Dataset: c.Dataset, StateDir: "relative/dir"},
		{App: c.App, Dataset: c.Dataset, StateDir: c.StateDir, Channel: "v1.2.3"},
	} {
		if err := bad.WithDefaults().Validate(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	if err := os.WriteFile(p, []byte(`{"app":"ai-minion","dataset":"a/b","state_dir":"/x/y"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadConfig(p, os.Getuid()); err != nil || got.App != "ai-minion" {
		t.Fatalf("%+v %v", got, err)
	}
	if err := os.Chmod(p, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p, -1); err == nil {
		t.Fatal("group/world-writable config accepted")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"app":"a","unknown":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p, -1); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestJournalRefusesOpenDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // the umask may have narrowed it
		t.Fatal(err)
	}
	if _, err := OpenJournal(dir, "ai-minion", -1, nil); err == nil {
		t.Fatal("a state directory open to group and others was accepted")
	}
}
