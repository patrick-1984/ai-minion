//go:build !windows

package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// rootSocketDirSafe checks, for a process of root only, that no other account
// can change the path of the tmux socket: the directory and every directory
// above it belong to root and only root can write them, except a sticky
// directory (/tmp), where nobody else can rename root's entries. A missing
// socket directory is created so (mode 0700). Otherwise another account could
// put its own tmux server in the Worker's way.
func rootSocketDirSafe(dir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return fmt.Errorf("tmux: create the socket directory %s: %w", dir, err)
		}
	}
	bad := func(d string) error {
		return fmt.Errorf("tmux: %s can be changed by another account, so a Worker that runs as root refuses the tmux socket in %s "+
			"(use the default socket, or a directory like /run/minion-tmux that only root can change)", d, dir)
	}
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		fi, err := os.Lstat(d)
		if err != nil {
			return fmt.Errorf("tmux: socket directory %s: %w", d, err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !fi.IsDir() || !ok || st.Uid != 0 {
			return bad(d)
		}
		writable := fi.Mode().Perm()&0o022 != 0
		if writable && (d == filepath.Clean(dir) || fi.Mode()&os.ModeSticky == 0) {
			return bad(d)
		}
		if d == "/" {
			return nil
		}
	}
}
