//go:build !windows

package tmux

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// rootSocketDirSafe checks, for a process of root only, that the directory of
// the tmux socket belongs to root and is writable by root alone; a missing
// directory is created so (mode 0700). Any other account that could write
// there could put its own tmux server in the Worker's way.
func rootSocketDirSafe(dir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return fmt.Errorf("tmux: create the socket directory %s: %w", dir, err)
		}
		fi, err = os.Lstat(dir)
	}
	if err != nil {
		return fmt.Errorf("tmux: socket directory %s: %w", dir, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("tmux: the socket directory %s is not a directory of root that only root can write; "+
			"a Worker that runs as root refuses it (use the default socket, or a directory like /run/minion-tmux with mode 0700)", dir)
	}
	return nil
}
