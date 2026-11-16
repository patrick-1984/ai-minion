package version

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path through a temporary file in the same
// directory, so a crash never leaves a half-written file.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if err := checkNewFile(tmp); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// ReplaceBinary atomically replaces the executable at target with the file at
// newBinary. The new file is first copied next to the target (a rename across
// file systems is not atomic), synced and then renamed over it, so a reader
// sees either the old or the new binary. A symlink target is resolved first.
//
// wantSHA256 is the digest of the executable that was verified (extracted from
// an archive whose signed digest matched). newBinary is opened once, without
// following a link (see [openTrusted]), and the bytes copied are hashed on the
// way: when they do not hash to wantSHA256 the copy is discarded, the target is
// not touched and the error wraps [ErrChecksum]. A path that was swapped after
// the verification therefore never gets installed. An empty wantSHA256 is
// refused: there is no unverified install.
//
// The previous executable is kept as "<target>.old" until the next update, so
// that a Worker can roll back (on Windows the running executable is renamed,
// elsewhere hard-linked).
func ReplaceBinary(target, newBinary, goos, wantSHA256 string) error {
	if len(wantSHA256) != sha256.Size*2 {
		return errors.New("refusing to install an executable without its verified SHA-256")
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("cannot locate the running binary: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", real)
	}

	// The temporary copy, and on Windows the renamed old executable, are
	// created in the directory of the target: it and the way to it must not be
	// changeable by anyone less trusted than the user that runs the update, or
	// the verified bytes would be placed where someone else controls them.
	if err := checkInstallDir(filepath.Dir(real)); err != nil {
		if errors.Is(err, ErrUntrustedCache) {
			return fmt.Errorf("cannot install into %q: %w; nothing was installed. Install minion into a directory that only you or an administrator can change", filepath.Dir(real), err)
		}
		return replaceError(real, err)
	}
	src, err := openTrusted(newBinary)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(real), ".minion-update-*")
	if err != nil {
		return replaceError(real, err)
	}
	tmpName := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := checkNewFile(tmp); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(src, maxBinarySize+1))
	if err != nil {
		return err
	}
	if n > maxBinarySize {
		return errors.New("the new executable is too large")
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA256 {
		return fmt.Errorf("%w: the executable to install has sha256 %s, the verified release has %s; nothing was installed", ErrChecksum, got, wantSHA256)
	}
	if err := tmp.Chmod(info.Mode().Perm()&^0o022 | 0o111); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if goos == "windows" {
		old := real + ".old"
		_ = os.Remove(old)
		if err := os.Rename(real, old); err != nil {
			return replaceError(real, err)
		}
		if err := os.Rename(tmpName, real); err != nil {
			// Put the old binary back so the installation stays usable.
			_ = os.Rename(old, real)
			return replaceError(real, err)
		}
		done = true
		return nil
	}
	// Keep the previous executable as "<target>.old" (a hard link, so the
	// running process is not disturbed): a Worker whose new binary cannot
	// start restores it (crash guard). Best effort: without it the update
	// still installs, only that rollback is not possible.
	old := real + ".old"
	_ = os.Remove(old)
	if err := os.Link(real, old); err != nil {
		// No hard links here (some file systems): a copy keeps the rollback.
		if err := copyFile(real, old, info.Mode().Perm()); err != nil {
			_ = os.Remove(old)
			return fmt.Errorf("cannot keep the previous executable for a rollback, nothing was installed: %w", err)
		}
	}
	if err := os.Rename(tmpName, real); err != nil {
		_ = os.Remove(old)
		return replaceError(real, err)
	}
	done = true
	return nil
}

func replaceError(target string, err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("cannot replace %s: permission denied (run minion update with the rights that installed it, for example sudo): %w", target, err)
	}
	return fmt.Errorf("cannot replace %s: %w", target, err)
}

// copyFile copies src to a new file dst with mode perm.
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
