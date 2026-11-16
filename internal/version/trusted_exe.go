package version

import "path/filepath"

// CheckTrustedExecutable reports whether path is an executable that only the
// running identity, SYSTEM and the administrators can change: its directory
// chain passes the same checks as an install directory, and the file itself
// passes the checks of a new binary. On Unix it only checks that the path is
// absolute and a regular file (the Worker there never runs as another user's
// helper). It is used before the Worker runs a helper program it found
// itself, such as psmux, so that a Windows service (LocalSystem) never runs
// an executable another account could have planted.
func CheckTrustedExecutable(path string) error {
	if !filepath.IsAbs(path) {
		return errUntrusted(path + " is not an absolute path")
	}
	if err := checkInstallDir(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := openTrusted(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errUntrusted(path + " is not a regular file")
	}
	return checkNewFile(f)
}
