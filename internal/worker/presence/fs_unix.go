//go:build !windows

package presence

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func platformRenameRetryable(error) bool { return false }

// fileIdentity names a file by device and inode.
func fileIdentity(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino)) //nolint:unconvert // Dev and Ino differ per OS
}

func fileUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// checkAncestors walks every directory above the final component of abs (after
// resolving links of the parents) and refuses one that is owned by an untrusted
// uid or writable by group or others. Directories further up may be sticky
// (/tmp); the direct parent may not, because a sticky directory lets another
// user pre-create the name.
func checkAncestors(abs string, trusted func(uid int) bool) (string, error) {
	base := filepath.Base(abs)
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	cur := string(filepath.Separator)
	rest := strings.Split(strings.TrimPrefix(parent, cur), cur)
	for i := -1; i < len(rest); i++ {
		if i >= 0 {
			if rest[i] == "" {
				continue
			}
			cur = filepath.Join(cur, rest[i])
		}
		fi, err := os.Lstat(cur)
		if err != nil {
			return "", err
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("%s is not a directory", cur)
		}
		if uid, ok := fileUID(fi); !ok || !trusted(uid) {
			return "", fmt.Errorf("%s is owned by an untrusted user (uid %d)", cur, uid)
		}
		direct := cur == parent
		if fi.Mode().Perm()&0o022 != 0 && (direct || fi.Mode()&os.ModeSticky == 0) {
			return "", fmt.Errorf("%s is writable by group or others (mode %04o)", cur, fi.Mode().Perm())
		}
	}
	return filepath.Join(parent, base), nil
}

func trustedFor(uid int) func(int) bool { return func(u int) bool { return u == 0 || u == uid } }

// verifyPrivateDir checks an existing private directory: no link, owned by
// uid, no access for group or others, trusted ancestors. It returns the
// physical path.
func verifyPrivateDir(dir string, uid int) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	phys, err := checkAncestors(abs, trustedFor(uid))
	if err != nil {
		return "", err
	}
	fi, err := os.Lstat(phys)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() { // also refuses a symbolic link
		return "", fmt.Errorf("%s is not a directory (a link is refused)", phys)
	}
	if o, ok := fileUID(fi); !ok || o != uid {
		return "", fmt.Errorf("%s is owned by uid %d, not %d", phys, o, uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s has mode %04o; it must be private", phys, fi.Mode().Perm())
	}
	return phys, nil
}

// ensurePrivateDir creates dir (and missing parents) 0700 and verifies it. A
// presence directory that exists with a wider mode is tightened to 0700, never
// widened.
func ensurePrivateDir(dir string, uid int) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(abs, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if fi, err := os.Lstat(abs); err == nil && fi.IsDir() && fi.Mode().Perm()&0o077 != 0 {
		if o, ok := fileUID(fi); ok && o == uid {
			_ = os.Chmod(abs, 0o700)
		}
	}
	return verifyPrivateDir(abs, uid)
}

func verifyPublicDir(dir string, rootUID int) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	phys, err := checkAncestors(abs, func(u int) bool { return u == 0 || u == rootUID })
	if err != nil {
		return "", err
	}
	fi, err := os.Lstat(phys)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory (a link is refused)", phys)
	}
	if o, ok := fileUID(fi); !ok || o != rootUID {
		return "", fmt.Errorf("%s is owned by uid %d, not root (%d)", phys, o, rootUID)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("%s is writable by group or others (mode %04o)", phys, fi.Mode().Perm())
	}
	return phys, nil
}

// ensurePublicDirs prepares the public directory (root-owned, 0755) and its
// root-only staging directory (0700). It returns the physical paths.
func ensurePublicDirs(l Layout) (root, stage string, err error) {
	abs, err := filepath.Abs(l.PublicRoot)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", "", err
	}
	if err := os.Mkdir(abs, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", "", err
	}
	if fi, err := os.Lstat(abs); err == nil && fi.IsDir() {
		if o, ok := fileUID(fi); ok && o == l.RootUID && fi.Mode().Perm() != 0o755 && fi.Mode().Perm()&0o022 == 0 {
			if err := os.Chmod(abs, 0o755); err != nil {
				return "", "", err
			}
		}
	}
	if root, err = verifyPublicDir(abs, l.RootUID); err != nil {
		return "", "", err
	}
	stage = filepath.Join(root, stageDirName)
	if err := freshStage(root, stage, l.RootUID); err != nil {
		return "", "", err
	}
	if err := verifyStage(stage, l.RootUID); err != nil {
		return "", "", err
	}
	return root, stage, nil
}

var claimNameRE = regexp.MustCompile(`^profile-[0-9a-f]{16}$`)

// freshStage makes the root-only staging directory trustworthy before any use.
// A directory that is missing is created 0700. One that is not a directory
// owned by root with mode 0700 exactly (it may have been writable by others
// once, and may hold links planted in it) is moved aside whole and replaced by
// a new one. In a good directory every entry other than a root-owned private
// claim note (profile-<hex>), or a root-owned temporary file of a writer (tmp-*,
// of any mode: it may be in flight in another Worker; swept by age later), is
// removed.
func freshStage(root, stage string, rootUID int) error {
	fi, err := os.Lstat(stage)
	switch {
	case err != nil && !isNotExist(err):
		return err
	case err != nil:
		return mkStage(stage)
	}
	o, ok := fileUID(fi)
	if !fi.IsDir() || !ok || o != rootUID || fi.Mode().Perm() != 0o700 {
		var rnd [8]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return err
		}
		aside := filepath.Join(root, ".stage-old-"+hex.EncodeToString(rnd[:]))
		if err := os.Rename(stage, aside); err != nil && !isNotExist(err) {
			return fmt.Errorf("the staging directory %s cannot be trusted and cannot be moved aside: %w", stage, err)
		}
		if err := mkStage(stage); err != nil {
			return err
		}
		if fi.IsDir() && ok && o == rootUID {
			_ = os.Chmod(aside, 0o700)
		}
		_ = os.RemoveAll(aside)
		return nil
	}
	ents, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	for _, e := range ents {
		n := e.Name()
		efi, err := os.Lstat(filepath.Join(stage, n))
		if err != nil {
			continue
		}
		eo, _ := fileUID(efi)
		// A temporary file may be in flight in another Worker (it is chmodded
		// before its rename); the directory is root-only, so it exposes nothing
		// and sweepTemps removes it by age.
		keep := efi.Mode().IsRegular() && eo == rootUID &&
			((claimNameRE.MatchString(n) && efi.Mode().Perm()&0o077 == 0) || strings.HasPrefix(n, "tmp-"))
		if !keep {
			if err := os.RemoveAll(filepath.Join(stage, n)); err != nil && !isNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// mkStage creates the staging directory. One that another Worker created in
// the meantime is fine: the caller verifies it.
func mkStage(stage string) error {
	if err := os.Mkdir(stage, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	return os.Chmod(stage, 0o700) // the umask must not matter
}

// readClaimFile reads a claim note of the staging directory through one
// checked handle: no link, a regular file owned by root, private.
func readClaimFile(stage, name string, rootUID int) ([]byte, error) {
	if err := verifyStage(stage, rootUID); err != nil {
		return nil, err
	}
	f, err := openChecked(stage, name, rootUID, func(m os.FileMode) bool { return m&0o077 == 0 })
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readBounded(f, 128)
}

// writeClaimFile writes a claim note: random temporary name, O_EXCL and
// O_NOFOLLOW, then rename.
func writeClaimFile(stage, name string, data []byte, rootUID int) error {
	if err := verifyStage(stage, rootUID); err != nil {
		return err
	}
	return atomicWrite(stage, stage, name, data, 0o600, nil)
}

func verifyStage(stage string, rootUID int) error {
	fi, err := os.Lstat(stage)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", stage)
	}
	if o, ok := fileUID(fi); !ok || o != rootUID || fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must be a root-only directory", stage)
	}
	return nil
}

// writePrivate writes dir/name atomically with mode 0600 (temporary file
// created 0600, fsync, rename, directory fsync).
func writePrivate(dir, name string, data []byte, uid int, gate func() bool) error {
	phys, err := verifyPrivateDir(dir, uid)
	if err != nil {
		return err
	}
	return atomicWrite(phys, phys, name, data, 0o600, gate)
}

// writePublic stages the record in the root-only directory and renames it into
// the public directory with mode 0644.
func writePublic(l Layout, name string, data []byte, gate func() bool) error {
	root, err := verifyPublicDir(l.PublicRoot, l.RootUID)
	if err != nil {
		return err
	}
	stage := filepath.Join(root, stageDirName)
	if err := verifyStage(stage, l.RootUID); err != nil {
		return err
	}
	if err := atomicWrite(stage, root, name, data, 0o644, gate); err != nil {
		return err
	}
	fi, err := os.Lstat(filepath.Join(root, name))
	if err != nil {
		return err
	}
	if o, ok := fileUID(fi); !ok || o != l.RootUID || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o644 {
		_ = os.Remove(filepath.Join(root, name))
		return fmt.Errorf("%s did not end with the expected owner and mode", name)
	}
	return nil
}

// atomicWrite creates a 0600 temporary file in stageDir, sets the final mode on
// the open handle (so the umask does not matter, and the file is not reachable
// before: stageDir is private), syncs it and renames it to destDir/name.
func atomicWrite(stageDir, destDir, name string, data []byte, mode os.FileMode, gate func() bool) error {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := filepath.Join(stageDir, "tmp-"+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := renameWithRetry(tmp, filepath.Join(destDir, name), gate); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(destDir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// openChecked opens dir/name once (no link followed, no blocking on a FIFO) and
// validates the handle: a regular file owned by owner whose mode satisfies
// okMode. The caller closes it.
func openChecked(dir, name string, owner int, okMode func(os.FileMode) bool) (*os.File, error) {
	p := filepath.Join(dir, name)
	var fd int
	var err error
	for {
		fd, err = unix.Open(p, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK) {
			return nil, fmt.Errorf("%s is a symbolic link", p)
		}
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	f := os.NewFile(uintptr(fd), p)
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	switch o, _ := fileUID(fi); {
	case !fi.Mode().IsRegular():
		err = fmt.Errorf("%s is not a regular file", p)
	case o != owner:
		err = fmt.Errorf("%s is owned by uid %d, expected %d", p, o, owner)
	case !okMode(fi.Mode().Perm()):
		err = fmt.Errorf("%s has unexpected mode %04o", p, fi.Mode().Perm())
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func readPrivateFile(dir, name string, uid int, max int64) ([]byte, error) {
	f, err := openChecked(dir, name, uid, func(m os.FileMode) bool { return m&0o077 == 0 })
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readBounded(f, max)
}

func readPublicFile(dir, name string, rootUID int, max int64) ([]byte, error) {
	f, err := openChecked(dir, name, rootUID, func(m os.FileMode) bool { return m == 0o644 })
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readBounded(f, max)
}

// sweepStage removes abandoned temporary files older than a minute.
func sweepTemps(dir string, now time.Time) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "tmp-") && (!strings.HasPrefix(n, ".") || !strings.Contains(n, ".tmp-")) {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() && now.Sub(fi.ModTime()) > time.Minute {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
}
