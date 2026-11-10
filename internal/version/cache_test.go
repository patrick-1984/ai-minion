package version

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheTags(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"v1.2.3", "v1.10.0", "v1.2.4-rc.1", "v0.9.0", ".stage-abc", "notes", "1.2.3", "v1.2"} {
		if err := os.Mkdir(filepath.Join(dir, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(dir, "v9.9.9"), "a file, not an entry", 0o600)
	tags, err := Cache{Dir: dir}.Tags()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range tags {
		got = append(got, v.String())
	}
	if want := "v1.10.0 v1.2.4-rc.1 v1.2.3 v0.9.0"; strings.Join(got, " ") != want {
		t.Errorf("Tags = %v, want %s", got, want)
	}
	if tags, err := (Cache{Dir: filepath.Join(dir, "missing")}).Tags(); err != nil || len(tags) != 0 {
		t.Errorf("missing cache: %v, %v", tags, err)
	}
}

func TestCacheCommitReplacesEntry(t *testing.T) {
	c := Cache{Dir: filepath.Join(t.TempDir(), "cache")}
	for _, content := range []string{"first", "second"} {
		st, err := c.NewStaging()
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(st, "minion"), content, 0o700)
		if err := c.Commit(st, "v1.0.0"); err != nil {
			t.Fatal(err)
		}
	}
	if got := readFile(t, filepath.Join(c.EntryDir("v1.0.0"), "minion")); got != "second" {
		t.Errorf("entry = %q", got)
	}
	if _, err := (Cache{}).NewStaging(); err == nil {
		t.Error("an empty cache directory was accepted")
	}
}
