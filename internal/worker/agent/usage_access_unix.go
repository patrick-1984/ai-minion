//go:build unix

package agent

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"golang.org/x/sys/unix"
)

// workerUID is the user the Worker runs as (tests replace it).
var workerUID = os.Getuid

// procRoot is where process directories live (tests replace it).
var procRoot = "/proc"

func openOwnedProc(pid int) *os.File {
	dir, err := os.Open(procRoot + "/" + strconv.Itoa(pid))
	if err != nil {
		return nil
	}
	if info, err := dir.Stat(); err == nil && info.IsDir() {
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) == workerUID() {
			return dir
		}
	}
	_ = dir.Close()
	return nil
}

func readProcFile(dir *os.File, name string, limit int64) ([]byte, bool) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	return raw, err == nil && int64(len(raw)) <= limit
}

// Open environment and argv relative to the UID-checked directory so the
// access evidence cannot mix two processes when a PID is reused.
func readProcAccess(pid int, claude bool) ([]byte, claudeSrc, bool) {
	dir := openOwnedProc(pid)
	if dir == nil {
		return nil, claudeSrc{}, false
	}
	defer func() { _ = dir.Close() }()
	raw, ok := readProcFile(dir, "environ", processEnvLimit)
	var src claudeSrc
	if ok && claude {
		src, _ = procClaudeSrc(dir)
	}
	return raw, src, ok
}

// procClaudeSrc reads the --settings argument and working directory of the
// Claude process behind dir. A relative settings file is resolved in that
// working directory; an inline JSON argument is returned as is (it is only
// reduced to presence flags by the caller).
func procClaudeSrc(dir *os.File) (claudeSrc, bool) {
	raw, ok := readProcFile(dir, "cmdline", 64<<10)
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if !ok || agentdetect.ProcessHarness(args) != "claude" {
		return claudeSrc{}, false
	}
	var src claudeSrc
	var cwd [4096]byte
	if n, err := unix.Readlinkat(int(dir.Fd()), "cwd", cwd[:]); err == nil && n < len(cwd) && filepath.IsAbs(string(cwd[:n])) {
		src.Cwd = string(cwd[:n])
	}
	var file string
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if v, ok := strings.CutPrefix(arg, "--settings="); ok {
			file = v
		} else if arg == "--settings" && i+1 < len(args) {
			i++
			file = args[i]
		}
	}
	switch {
	case file == "" || strings.HasPrefix(file, "-"):
	case strings.HasPrefix(strings.TrimSpace(file), "{"), filepath.IsAbs(file):
		src.Settings = file
	case src.Cwd != "":
		src.Settings = filepath.Join(src.Cwd, file)
	}
	return src, true
}

func parentClaudeSrc() claudeSrc {
	src := claudeAncestorSrc(os.Getppid())
	if src.Cwd == "" {
		src.Cwd, _ = os.Getwd()
	}
	return src
}

// Exporters may be launched through a shell. Look for the closest Claude
// process in a bounded chain of same-user ancestors.
func claudeAncestorSrc(pid int) claudeSrc {
	for range 8 {
		if pid <= 1 {
			break
		}
		dir := openOwnedProc(pid)
		if dir == nil {
			break
		}
		src, claude := procClaudeSrc(dir)
		if claude {
			_ = dir.Close()
			return src
		}
		raw, ok := readProcFile(dir, "stat", 4096)
		_ = dir.Close()
		s := string(raw)
		i := strings.LastIndexByte(s, ')')
		if !ok || i < 0 {
			break
		}
		fields := strings.Fields(s[i+1:])
		if len(fields) < 2 {
			break
		}
		pid, _ = strconv.Atoi(fields[1])
	}
	return claudeSrc{}
}

// openNonBlocking opens a file without waiting for a writer, so that a named
// pipe cannot stall the caller.
func openNonBlocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
