package version

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeFile(t testing.TB, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

func readFile(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReplaceBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "minion")
	next := filepath.Join(dir, "new")
	writeFile(t, target, "old", 0o755)
	writeFile(t, next, "new", 0o600)

	if err := ReplaceBinary(target, next, "linux", sha256Hex("new")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, target); got != "new" {
		t.Fatalf("target = %q", got)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(target)
		if st.Mode().Perm()&0o111 == 0 {
			t.Errorf("replaced binary is not executable: %v", st.Mode())
		}
	}
	if got := readFile(t, next); got != "new" {
		t.Errorf("source file changed: %q", got)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() != "minion" && e.Name() != "new" {
			t.Errorf("stray file %s after update", e.Name())
		}
	}
}

func TestReplaceBinaryFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-minion")
	link := filepath.Join(dir, "mn")
	next := filepath.Join(dir, "new")
	writeFile(t, real, "old", 0o755)
	writeFile(t, next, "new", 0o755)
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := ReplaceBinary(link, next, "linux", sha256Hex("new")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, real); got != "new" {
		t.Errorf("real file = %q", got)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced by a file: %v %v", fi, err)
	}
}

// The Windows branch moves the running file aside; the logic is portable.
func TestReplaceBinaryWindowsBranch(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "minion.exe")
	next := filepath.Join(dir, "new")
	writeFile(t, target, "old", 0o755)
	writeFile(t, next, "new", 0o755)
	// A stale .old from an earlier update must not block the next one.
	writeFile(t, target+".old", "stale", 0o755)

	if err := ReplaceBinary(target, next, "windows", sha256Hex("new")); err != nil {
		t.Fatal(err)
	}
	if readFile(t, target) != "new" || readFile(t, target+".old") != "old" {
		t.Errorf("target = %q, .old = %q", readFile(t, target), readFile(t, target+".old"))
	}
}

func TestReplaceBinaryErrors(t *testing.T) {
	dir := t.TempDir()
	next := filepath.Join(dir, "new")
	writeFile(t, next, "new", 0o755)
	if err := ReplaceBinary(filepath.Join(dir, "missing"), next, "linux", sha256Hex("new")); err == nil {
		t.Error("a missing target was accepted")
	}
	if err := ReplaceBinary(dir, next, "linux", sha256Hex("new")); err == nil {
		t.Error("a directory target was accepted")
	}
	target := filepath.Join(dir, "minion")
	writeFile(t, target, "old", 0o755)
	if err := ReplaceBinary(target, filepath.Join(dir, "gone"), "linux", sha256Hex("new")); err == nil {
		t.Error("a missing source was accepted")
	}
	if got := readFile(t, target); got != "old" {
		t.Errorf("a failed replace changed the target: %q", got)
	}
}

func TestReplaceBinaryReadOnlyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not apply")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "bin")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(sub, "minion")
	next := filepath.Join(dir, "new")
	writeFile(t, target, "old", 0o755)
	writeFile(t, next, "new", 0o755)
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	err := ReplaceBinary(target, next, "linux", sha256Hex("new"))
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want a permission hint", err)
	}
	if readFile(t, target) != "old" {
		t.Error("target changed")
	}
}

// The bytes copied are checked against the verified digest before anything is
// renamed over the target: a file that was swapped after verification, or a
// call without a digest, installs nothing.
func TestReplaceBinaryRefusesUnverifiedBytes(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "minion")
	next := filepath.Join(dir, "new")
	writeFile(t, target, "old", 0o755)
	writeFile(t, next, "swapped", 0o755)

	if err := ReplaceBinary(target, next, "linux", sha256Hex("new")); !errors.Is(err, ErrChecksum) {
		t.Fatalf("digest mismatch: err = %v", err)
	}
	for _, want := range []string{"", "abc", strings.Repeat("g", 64)} {
		if err := ReplaceBinary(target, next, "linux", want); err == nil {
			t.Errorf("digest %q was accepted", want)
		}
	}
	if got := readFile(t, target); got != "old" {
		t.Errorf("target = %q", got)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() != "minion" && e.Name() != "new" {
			t.Errorf("stray file %s after a refused update", e.Name())
		}
	}
}
