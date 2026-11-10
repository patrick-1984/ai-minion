package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Chowner changes the owner of files without following symbolic links. It is
// the port that lets tests observe ownership changes without being root.
type Chowner interface {
	// ChownDir gives an open directory to uid and gid.
	ChownDir(dir *os.File, uid, gid int) error
	// Lchown gives name, relative to root, to uid and gid. The last component
	// is never followed.
	Lchown(root *os.Root, name string, uid, gid int) error
}

// OSChowner is the [Chowner] of the operating system.
type OSChowner struct{}

// ChownDir implements [Chowner].
func (OSChowner) ChownDir(dir *os.File, uid, gid int) error { return dir.Chown(uid, gid) }

// Lchown implements [Chowner].
func (OSChowner) Lchown(root *os.Root, name string, uid, gid int) error {
	return root.Lchown(name, uid, gid)
}

// DirSpec describes a directory that `worker up` opens as root on behalf of
// another user.
type DirSpec struct {
	// Path is the absolute path of the directory.
	Path string
	// Create makes the missing directories of Path with Mode.
	Create bool
	Mode   fs.FileMode
	// UID and GID receive every directory that was created, through Chown.
	UID, GID int
	Chown    Chowner
	// LinkOwner is the only user whose symbolic links in the ancestry of Path
	// are followed, and only inside a directory that this user owns and that
	// nobody else can write to: the zero value is root. A link anybody else
	// could have planted or replaced is refused.
	LinkOwner int
}

// OpenTrustedDir opens spec.Path for a process that runs as root on behalf of
// another user. That user controls the directories below their home, so a
// path-based check is not enough: they can replace a directory by a symbolic
// link between the check and the use and make root reach another account.
//
// The whole path, from the file system root, is walked one component at a time
// through directory handles that never follow a link; the only links followed
// are those that [DirSpec.LinkOwner] owns inside a directory that only that
// user can write (/var/run to /run, /tmp to /private/tmp). The returned Root
// holds a handle on the directory itself: every operation through it stays
// inside the directory, whatever the user does to the path afterwards, so
// callers must work only through the Root, never through spec.Path.
func OpenTrustedDir(spec DirSpec) (*os.Root, error) {
	if !filepath.IsAbs(spec.Path) {
		return nil, fmt.Errorf("%s is not an absolute path", spec.Path)
	}
	spec.Path = filepath.Clean(spec.Path)
	verified, err := walkTrusted(spec)
	if err != nil {
		return nil, err
	}
	defer func() { _ = verified.Close() }()
	root, err := os.OpenRoot(spec.Path)
	if err != nil {
		return nil, err
	}
	want, err := verified.Stat()
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	got, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !os.SameFile(want, got) {
		_ = root.Close()
		return nil, fmt.Errorf("%s changed while it was being opened; refusing to continue as root", spec.Path)
	}
	return root, nil
}

// ChownTree gives the directory of root and everything below it to uid and
// gid. Every change goes through root, so a directory that is replaced by a
// link during the walk cannot lead outside; symbolic links are changed
// themselves, never followed.
func ChownTree(root *os.Root, uid, gid int, ch Chowner) error {
	return fs.WalkDir(root.FS(), ".", func(name string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return ch.Lchown(root, name, uid, gid)
	})
}

func errNotDirectory(p string) error {
	return fmt.Errorf("%s is not a directory", p)
}

func errLink(p string) error {
	return fmt.Errorf("%s is a symbolic link that this user does not control the placement of; refusing to follow it while running as root", p)
}

// isNotExist reports a missing path.
func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" && s != "." {
			out = append(out, s)
		}
	}
	return out
}

// WriteFileIn writes content to name in root (replacing it atomically) and,
// when ch is not nil, gives it to uid and gid. The file keeps its content and
// mode when it holds exactly content already.
func WriteFileIn(root *os.Root, name, content string, mode fs.FileMode, uid, gid int, ch Chowner) error {
	if f, err := root.Open(name); err == nil {
		data, rerr := io.ReadAll(io.LimitReader(f, int64(len(content))+1))
		_ = f.Close()
		if rerr == nil && string(data) == content {
			return chownIn(root, name, uid, gid, ch)
		}
	}
	var tmp string
	var f *os.File
	for i := 0; ; i++ {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		tmp = ".minion-" + hex.EncodeToString(b[:]) + ".tmp"
		var err error
		f, err = root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) || i > 10 {
			return err
		}
	}
	fail := func(err error) error {
		_ = f.Close()
		_ = root.Remove(tmp)
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		return fail(err)
	}
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return chownIn(root, name, uid, gid, ch)
}

func chownIn(root *os.Root, name string, uid, gid int, ch Chowner) error {
	if ch == nil {
		return nil
	}
	return ch.Lchown(root, name, uid, gid)
}
