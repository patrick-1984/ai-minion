//go:build !windows

package tmux

import (
	"os"
	"syscall"
)

// ownedByMe reports whether fi belongs to the user of this process.
func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
