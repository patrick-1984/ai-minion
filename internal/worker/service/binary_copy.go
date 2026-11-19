package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/version"
)

// ErrBinaryDigest is returned when the bytes copied for the service do not
// hash to the verified digest.
var ErrBinaryDigest = errors.New("the service binary does not match the verified release")

// ErrServiceDowngrade refuses to replace the service binary with an older
// release (the Hub may have updated it past the CLI that runs "worker up").
var ErrServiceDowngrade = errors.New("the service runs a newer release")

// maxServiceBinarySize bounds the copy (the release binary is far smaller).
const maxServiceBinarySize = 512 << 20

// BinaryInstall is the binary to put in place for the Windows service.
type BinaryInstall struct {
	// Src is the file to copy; SHA256 its verified digest ("" for an
	// explicitly unverified install).
	Src, SHA256 string
	// Version is the release Src is ("" when unknown: no downgrade check).
	Version string
	// AllowDowngrade replaces a newer installed release.
	AllowDowngrade bool
}

// replaceAndActivate puts bi in place at dst and runs activate (stop,
// reconfigure, start the service), all under the lock that "minion update"
// and the Worker's remote update take for dst, so that no other installer
// changes dst between the copy and the start. The previous binary is kept as
// dst.previous until activate succeeds; when activate fails it is put back.
// A newer installed release is not replaced unless bi.AllowDowngrade.
// installed reads the release of the binary at a path (false: unknown).
func replaceAndActivate(dst string, bi BinaryInstall, installed func(string) (version.SemVer, bool), activate func(dst string) error) error {
	release, err := version.LockBinary(dst)
	if err != nil {
		return err
	}
	defer release()
	if !bi.AllowDowngrade && bi.Version != "" && installed != nil {
		if _, err := os.Lstat(dst); err == nil {
			want, werr := version.ParseVersion(bi.Version)
			if cur, ok := installed(dst); ok && werr == nil && cur.Compare(want) > 0 {
				return fmt.Errorf("%w: %s is %s, this would install %s; update the service from the Hub, run the newer minion, or pass --allow-service-downgrade",
					ErrServiceDowngrade, dst, cur, bi.Version)
			}
		}
	}
	prev, err := copyServiceBinary(bi.Src, dst, bi.SHA256)
	if err != nil {
		return err
	}
	if err := activate(dst); err != nil {
		if prev != "" {
			if rerr := putBack(dst, prev); rerr != nil {
				return errors.Join(err, fmt.Errorf("cannot put the previous binary back (it is %s): %w", prev, rerr))
			}
		}
		return err
	}
	if prev != "" {
		retire(prev)
	}
	return nil
}

// putBack moves the new dst aside and the previous binary back to dst.
func putBack(dst, prev string) error {
	aside := filepath.Join(filepath.Dir(dst), fmt.Sprintf("%s%d", replacedPrefix, time.Now().UnixNano()))
	if err := os.Rename(dst, aside); err != nil {
		return err
	}
	if err := os.Rename(prev, dst); err != nil {
		_ = os.Rename(aside, dst)
		return err
	}
	_ = os.Remove(aside)
	return nil
}

// retire removes a replaced binary, or, while it still runs, renames it to a
// scratch name that a later install removes.
func retire(p string) {
	if err := os.Remove(p); err == nil || os.IsNotExist(err) {
		return
	}
	_ = os.Rename(p, filepath.Join(filepath.Dir(p), fmt.Sprintf("%s%d", replacedPrefix, time.Now().UnixNano())))
}

// copyServiceBinary copies src next to dst (so the copy inherits the DACL of
// the directory) and renames it over dst. With wantSHA256 the bytes are
// hashed while they are copied, and a copy that does not match is discarded
// before dst is touched: src may be in a directory the user can change, and
// only the copied bytes count. The binary dst held is moved to dst.previous
// (a running binary cannot be deleted on Windows but can be renamed) and its
// path returned, "" when there was none. dst.old, the previous release that
// the remote update keeps for its crash rollback, is never touched. The
// caller holds the update lock of dst.
func copyServiceBinary(src, dst, wantSHA256 string) (prev string, err error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	if fi, err := in.Stat(); err != nil {
		return "", err
	} else if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", src)
	}
	dir := filepath.Dir(dst)
	removeReplaced(dir)
	tmp, err := os.CreateTemp(dir, ".minion-install-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(in, maxServiceBinarySize+1))
	if err != nil {
		return "", err
	}
	if n > maxServiceBinarySize {
		return "", fmt.Errorf("%s is too large for a minion binary", src)
	}
	if wantSHA256 != "" {
		if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA256 {
			return "", fmt.Errorf("%w: %s has sha256 %s, the verified release has %s; nothing was installed", ErrBinaryDigest, src, got, wantSHA256)
		}
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	// src may be dst itself (an unverified install from the copy): Windows
	// does not rename a file that is open without FILE_SHARE_DELETE.
	_ = in.Close()
	if _, err := os.Lstat(dst); err == nil {
		prev = dst + previousSuffix
		if _, err := os.Lstat(prev); err == nil {
			retire(prev)
		}
		if err := os.Rename(dst, prev); err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmpName, dst); err != nil {
		if prev != "" {
			_ = os.Rename(prev, dst)
		}
		return "", err
	}
	done = true
	return prev, nil
}

// previousSuffix names the binary an install replaced, kept until the new one
// runs.
const previousSuffix = ".previous"

// replacedPrefix names a replaced service binary that still ran.
const replacedPrefix = ".minion-replaced-"

// removeReplaced removes replaced binaries that no longer run.
func removeReplaced(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), replacedPrefix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// isOwnFile reports whether name is a file the installer or the updater
// creates in the service binary directory.
func isOwnFile(name string) bool {
	switch name {
	case serviceBinaryName, serviceBinaryName + ".update-lock", serviceBinaryName + previousSuffix, serviceBinaryName + ".old":
		return true
	}
	for _, p := range []string{replacedPrefix, ".minion-install-", ".minion-update-", serviceBinaryName + ".old-"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
