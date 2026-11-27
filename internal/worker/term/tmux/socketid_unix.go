//go:build !windows

package tmux

import (
	"fmt"
	"os"
	"syscall"
)

// socketIdentity names the socket file at path by device, inode and change
// time, so a server that recreated its socket at the same path differs.
func socketIdentity(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("no file identity for the tmux socket")
	}
	return fmt.Sprintf("%d:%d %d", uint64(st.Dev), uint64(st.Ino), ctimeNano(st)), nil
}
