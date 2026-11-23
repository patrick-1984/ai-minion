//go:build unix

package agent

import (
	"os"
	"strconv"
	"syscall"
)

// workerUID is the user the Worker runs as (tests replace it).
var workerUID = os.Getuid

// sameUserProcess reports whether /proc/<pid> belongs to this Worker's user:
// the environment of another user's process is never read.
func sameUserProcess(pid int) bool {
	info, err := os.Stat("/proc/" + strconv.Itoa(pid))
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == workerUID()
}

// openNonBlocking opens a file without waiting for a writer, so that a named
// pipe cannot stall the caller.
func openNonBlocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
