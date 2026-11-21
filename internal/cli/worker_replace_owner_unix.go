//go:build unix

package cli

import (
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether path belongs to the user that runs the
// command and nobody else can write to it.
func ownedByCurrentUser(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid() && info.Mode().Perm()&0o022 == 0
}
