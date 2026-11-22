//go:build windows

package presence

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

func platformRenameRetryable(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

func fileIdentity(os.FileInfo) string { return "" }

func readClaimFile(string, string, int) ([]byte, error) { return nil, ErrUnsupported }
func writeClaimFile(string, string, []byte, int) error  { return ErrUnsupported }

// The private directory is created below the user's state directory and
// inherits its protected DACL; files get their own protected DACL at creation.
func verifyPrivateDir(dir string, _ int) (string, error) {
	if err := identity.CheckPrivateDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func ensurePrivateDir(dir string, uid int) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return verifyPrivateDir(dir, uid)
}

func verifyPublicDir(string, int) (string, error)           { return "", ErrUnsupported }
func ensurePublicDirs(Layout) (string, string, error)       { return "", "", ErrUnsupported }
func writePublic(Layout, string, []byte, func() bool) error { return ErrUnsupported }

func readPublicFile(string, string, int, int64) ([]byte, error) { return nil, ErrUnsupported }

func writePrivate(dir, name string, data []byte, uid int, gate func() bool) error {
	if _, err := verifyPrivateDir(dir, uid); err != nil {
		return err
	}
	tmp, err := identity.CreatePrivateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if gate != nil && !gate() {
		_ = os.Remove(tmpName)
		return errGateClosed
	}
	if err := renameWithRetry(tmpName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func readPrivateFile(dir, name string, _ int, max int64) ([]byte, error) {
	f, err := identity.OpenPrivate(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readBounded(f, max)
}

func sweepTemps(dir string, now time.Time) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		n := e.Name()
		if len(n) > 0 && n[0] == '.' {
			if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() && now.Sub(fi.ModTime()) > time.Minute {
				_ = os.Remove(filepath.Join(dir, n))
			}
		}
	}
}
