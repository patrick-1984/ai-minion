//go:build unix

package agent

import (
	"io"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// workerUID is the user the Worker runs as (tests replace it).
var workerUID = os.Getuid

// procRoot is where process directories live (tests replace it).
var procRoot = "/proc"

// readProcEnviron reads the environment block of a process of this Worker's
// user. The process directory is opened once, its owner is read from that
// open directory and the environment is opened relative to it: the user check
// and the read cannot land on two different processes if the pid is reused,
// and another user's environment is never opened.
func readProcEnviron(pid int) ([]byte, bool) {
	dir, err := os.Open(procRoot + "/" + strconv.Itoa(pid))
	if err != nil {
		return nil, false
	}
	defer func() { _ = dir.Close() }()
	info, err := dir.Stat()
	if err != nil || !info.IsDir() {
		return nil, false
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != workerUID() {
		return nil, false
	}
	fd, err := unix.Openat(int(dir.Fd()), "environ", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	f := os.NewFile(uintptr(fd), "environ")
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, processEnvLimit))
	return raw, err == nil
}

// openNonBlocking opens a file without waiting for a writer, so that a named
// pipe cannot stall the caller.
func openNonBlocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
