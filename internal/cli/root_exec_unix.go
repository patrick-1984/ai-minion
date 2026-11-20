//go:build !windows

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// checkRootExecutable checks a binary that root will run later (a root
// service, root's login startup): the file and every directory above it
// belong to root and only root can write them. Otherwise another account could
// replace it, also after a cleanup removed a directory on the way (a sticky
// /tmp is writable by everyone, so it fails here).
func checkRootExecutable(path string) error {
	fix := "install minion in /usr/local/bin as root (the installer does this with sudo), then run /usr/local/bin/minion worker up"
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("cannot check %s for a Worker that runs as root: %w; %s", path, err, fix)
	}
	info, err := os.Stat(real)
	if err != nil {
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("a Worker that runs as root needs a minion binary that only root can change, and %s is not; %s", real, fix)
	}
	for dir := filepath.Dir(real); ; dir = filepath.Dir(dir) {
		di, err := os.Stat(dir)
		if err != nil {
			return err
		}
		if st, ok := di.Sys().(*syscall.Stat_t); !ok || st.Uid != 0 || di.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("a Worker that runs as root needs a minion binary in directories that only root can change, and %s is not; %s", dir, fix)
		}
		if dir == "/" {
			return nil
		}
	}
}
