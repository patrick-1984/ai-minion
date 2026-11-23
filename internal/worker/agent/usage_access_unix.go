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
func readProcAccess(pid int, claude bool) ([]byte, string, bool) {
	dir := openOwnedProc(pid)
	if dir == nil {
		return nil, "", false
	}
	defer func() { _ = dir.Close() }()
	raw, ok := readProcFile(dir, "environ", processEnvLimit)
	var settings string
	if ok && claude {
		settings, _ = procClaudeSettings(dir)
	}
	return raw, settings, ok
}

func procClaudeSettings(dir *os.File) (string, bool) {
	raw, ok := readProcFile(dir, "cmdline", 64<<10)
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if !ok || agentdetect.ProcessHarness(args) != "claude" {
		return "", false
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
	if file == "" || strings.HasPrefix(file, "-") || strings.HasPrefix(strings.TrimSpace(file), "{") {
		return "", true // no file, or an inline settings JSON argument
	}
	if filepath.IsAbs(file) {
		return file, true
	}
	// Resolve a relative flag in the Claude process's own working directory.
	var cwd [4096]byte
	n, err := unix.Readlinkat(int(dir.Fd()), "cwd", cwd[:])
	if err != nil || n == len(cwd) || !filepath.IsAbs(string(cwd[:n])) {
		return "", true
	}
	return filepath.Join(string(cwd[:n]), file), true
}

func parentClaudeSettings() string {
	return claudeAncestorSettings(os.Getppid())
}

// Exporters may be launched through a shell. Look for the closest Claude
// process in a bounded chain of same-user ancestors.
func claudeAncestorSettings(pid int) string {
	for range 8 {
		if pid <= 1 {
			break
		}
		dir := openOwnedProc(pid)
		if dir == nil {
			break
		}
		file, claude := procClaudeSettings(dir)
		if claude {
			_ = dir.Close()
			return file
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
	return ""
}

// openNonBlocking opens a file without waiting for a writer, so that a named
// pipe cannot stall the caller.
func openNonBlocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
