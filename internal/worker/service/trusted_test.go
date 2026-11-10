//go:build unix

package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func spec(path string, ch Chowner) DirSpec {
	return DirSpec{Path: path, Create: true, Mode: 0o700, UID: 7, GID: 8, Chown: ch, LinkOwner: -1}
}

func TestOpenTrustedDirCreatesAndGivesEveryCreatedDirectoryOnly(t *testing.T) {
	base := t.TempDir()
	deep := filepath.Join(base, ".local", "state", "minion")
	rec := &recChowner{t: t, uid: 7, gid: 8}
	root, err := OpenTrustedDir(spec(deep, rec))
	if err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	want := []string{filepath.Join(base, ".local"), filepath.Join(base, ".local", "state"), deep}
	if strings.Join(rec.got, "|") != strings.Join(want, "|") {
		t.Fatalf("chowned %v, want %v", rec.got, want)
	}
	if st, err := os.Stat(deep); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", st, err)
	}
	// Existing directories are left alone.
	rec.got = nil
	root, err = OpenTrustedDir(spec(deep, rec))
	if err != nil || len(rec.got) != 0 {
		t.Fatalf("err = %v, chowned %v", err, rec.got)
	}
	_ = root.Close()
	// Without a Chowner it only creates.
	if root, err = OpenTrustedDir(spec(filepath.Join(base, "x", "y"), nil)); err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	// Without Create a missing directory is an error and nothing is made.
	s := spec(filepath.Join(base, "missing", "dir"), nil)
	s.Create = false
	if _, err := OpenTrustedDir(s); err == nil {
		t.Fatal("a missing directory was opened")
	}
	if _, err := os.Lstat(filepath.Join(base, "missing")); err == nil {
		t.Fatal("a directory was created without Create")
	}
	if _, err := OpenTrustedDir(spec("relative/path", nil)); err == nil {
		t.Fatal("a relative path was accepted")
	}
}

func TestOpenTrustedDirPropagatesChownError(t *testing.T) {
	ch := failChowner{}
	if _, err := OpenTrustedDir(spec(filepath.Join(t.TempDir(), "a"), ch)); err == nil {
		t.Fatal("chown error ignored")
	}
}

type failChowner struct{}

func (failChowner) ChownDir(*os.File, int, int) error       { return errors.New("denied") }
func (failChowner) Lchown(*os.Root, string, int, int) error { return errors.New("denied") }

// A custom state directory outside the target's home is checked as far as the
// file system root: a link anywhere in its ancestry that the target could have
// planted is refused, and nothing is read or written through it.
func TestOpenTrustedDirRefusesALinkedAncestorOutsideTheHome(t *testing.T) {
	shared := t.TempDir() // stands for /srv/shared
	other := t.TempDir()  // another account's directory
	if err := os.MkdirAll(filepath.Join(other, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(shared, "link")); err != nil {
		t.Fatal(err)
	}
	rec := &recChowner{t: t, uid: 7, gid: 8}
	for _, p := range []string{
		filepath.Join(shared, "link", "state"),        // existing directory behind the link
		filepath.Join(shared, "link", "state", "new"), // would be created behind the link
		filepath.Join(shared, "link"),                 // the link itself
		filepath.Join(shared, "link", "a", "b", "c"),  // several missing
	} {
		_, err := OpenTrustedDir(spec(p, rec))
		if err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("OpenTrustedDir(%s) = %v", p, err)
		}
	}
	if len(rec.got) != 0 {
		t.Errorf("something was given away: %v", rec.got)
	}
	entries, _ := os.ReadDir(filepath.Join(other, "state"))
	if _, err := os.Lstat(filepath.Join(other, "a")); err == nil || len(entries) != 0 {
		t.Errorf("something was created behind the link: %v", entries)
	}
}

func TestOpenTrustedDirFollowsOnlyLinksOfTheTrustedOwner(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(base, "rel")); err != nil { // relative target
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "abs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../escape", filepath.Join(base, "up")); err != nil {
		t.Fatal(err)
	}
	me := os.Getuid()
	trusted := func(p string) (*os.Root, error) {
		s := spec(p, nil)
		s.LinkOwner = me
		return OpenTrustedDir(s)
	}
	// The directory holding the links is ours and nobody else can write to it.
	for _, via := range []string{"rel", "abs"} {
		r, err := trusted(filepath.Join(base, via, "state"))
		if err != nil {
			t.Fatalf("via %s: %v", via, err)
		}
		_ = r.Close()
		if _, err := os.Stat(filepath.Join(base, "real", "state")); err != nil {
			t.Errorf("via %s: not created behind the link: %v", via, err)
		}
		_ = os.Remove(filepath.Join(base, "real", "state"))
	}
	if _, err := trusted(filepath.Join(base, "up", "state")); err == nil || !strings.Contains(err.Error(), "..") {
		t.Errorf("a link that climbs was followed: %v", err)
	}
	// Owned by somebody else than the trusted owner: refused.
	if _, err := OpenTrustedDir(spec(filepath.Join(base, "rel", "state"), nil)); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("a link of an untrusted owner was followed: %v", err)
	}
	// A directory that others can write to may hold a swapped link.
	if err := os.Chmod(base, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := trusted(filepath.Join(base, "rel", "state")); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("a link in a world-writable directory was followed: %v", err)
	}
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestOpenTrustedDirRefusesLoopsAndFiles(t *testing.T) {
	base := t.TempDir()
	if err := os.Symlink("b", filepath.Join(base, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(base, "b")); err != nil {
		t.Fatal(err)
	}
	s := spec(filepath.Join(base, "a", "x"), nil)
	s.LinkOwner = os.Getuid()
	if _, err := OpenTrustedDir(s); err == nil {
		t.Error("a link loop was accepted")
	}
	if err := os.WriteFile(filepath.Join(base, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTrustedDir(spec(filepath.Join(base, "file", "x"), nil)); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("a file as ancestor: %v", err)
	}
}

func TestChownTreeVisitsEverythingWithoutFollowingLinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "d", "f"), nil, 0o600)
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret"), nil, 0o600)
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	rec := &recChowner{t: t, uid: 1, gid: 2}
	if err := ChownTree(root, 1, 2, rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 4 { // dir, d, d/f, link
		t.Fatalf("seen %v", rec.got)
	}
	for _, p := range rec.got {
		if strings.HasPrefix(p, outside) {
			t.Errorf("followed the link into %s", p)
		}
	}
	// The real lchown works for the owner, on the directory itself too.
	if err := ChownTree(root, os.Getuid(), os.Getgid(), OSChowner{}); err != nil {
		t.Fatal(err)
	}
}

// The user can replace a directory by a link while root walks the tree: the
// walk has listed the directory and is about to descend. Every change goes
// through the root handle, so it never reaches the other account's file.
func TestChownTreeDoesNotFollowADirectorySwappedForALinkDuringTheWalk(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(dir, d, "inner"), 0o700); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, d, "inner", "f"), nil, 0o600)
	}
	outside := t.TempDir()
	secret := filepath.Join(outside, "inner", "f")
	if err := os.MkdirAll(filepath.Dir(secret), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	secretInfo, err := os.Stat(secret)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	swapped := false
	reached := ""
	rec := &recChowner{t: t, uid: 1, gid: 2}
	rec.onLchown = func(r *os.Root, name string) {
		if info, err := r.Stat(name); err == nil && os.SameFile(info, secretInfo) {
			reached = name
		}
		// "a" was just changed; the walk descends into it next. Replace it by a
		// link to the other account's directory.
		if name == "a" && !swapped {
			swapped = true
			if err := os.Rename(filepath.Join(dir, "a"), filepath.Join(dir, "a.moved")); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, "a")); err != nil {
				t.Error(err)
			}
		}
	}
	err = ChownTree(root, 1, 2, rec)
	if !swapped {
		t.Fatal("the swap did not happen")
	}
	if reached != "" {
		t.Fatalf("%s reached the other account's file", reached)
	}
	for _, p := range rec.got {
		if strings.HasPrefix(p, outside) {
			t.Errorf("changed %s outside the tree", p)
		}
	}
	// The walk is aborted: it was led away from the tree.
	if err == nil {
		t.Error("the walk went on after a directory was swapped for a link out of the tree")
	}
}

func TestWriteFileInReplacesAndGivesTheFileAndNeverFollowsALink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "other.txt")
	if err := os.WriteFile(outside, []byte("other account"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "plist")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	rec := &recChowner{t: t, uid: 7, gid: 8}
	if err := WriteFileIn(root, "plist", "content", 0o644, 7, 8, rec); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(filepath.Join(dir, "plist"))
	if err != nil || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0o644 {
		t.Fatalf("plist: %v %v", st, err)
	}
	if data, _ := os.ReadFile(outside); string(data) != "other account" {
		t.Errorf("the linked file was written: %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "plist")); string(data) != "content" {
		t.Errorf("content = %q", data)
	}
	if want := filepath.Join(dir, "plist"); len(rec.got) != 1 || rec.got[0] != want {
		t.Errorf("chowned %v, want [%s]", rec.got, want)
	}
	// Unchanged content: nothing is rewritten, ownership is still given.
	rec.got = nil
	before, _ := os.Stat(filepath.Join(dir, "plist"))
	if err := WriteFileIn(root, "plist", "content", 0o644, 7, 8, rec); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(dir, "plist"))
	if !os.SameFile(before, after) || len(rec.got) != 1 {
		t.Errorf("rewritten or not given: %v", rec.got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temporary files left: %v", entries)
	}
}
