package agentdetect

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultProcessSource returns the process table reader of the platform: the
// /proc reader on Linux (no exec, nothing to install: a Worker in a container
// often has no ps, and BusyBox ps lacks the job-control columns), ps(1) on
// macOS and the BSDs.
func DefaultProcessSource() ProcessSource {
	if runtime.GOOS == "linux" {
		return ProcSource{}
	}
	return PSSource{}
}

// ProcSource reads the process table from a procfs tree without running any
// command: /proc/<pid>/stat gives the parent, the process group, the
// terminal's foreground group and the run state; /proc/<pid>/cmdline gives the
// exact NUL-separated argv. Unreadable or vanished pids are skipped. It never
// logs or stores the command lines it reads.
type ProcSource struct {
	// Root is the procfs directory; empty means /proc. Tests point it at a
	// fake tree.
	Root string
}

// maxCmdline bounds how much of one cmdline is read.
const maxCmdline = 64 << 10

// Processes implements ProcessSource.
func (s ProcSource) Processes(ctx context.Context) ([]Proc, error) {
	root := s.Root
	if root == "" {
		root = "/proc"
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var procs []Proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || !e.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dir := filepath.Join(root, e.Name())
		stat, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue
		}
		p, ok := parseProcStat(stat)
		if !ok {
			continue
		}
		args, err := readCmdline(filepath.Join(dir, "cmdline"))
		if err != nil {
			continue
		}
		p.PID = pid
		p.Args = args
		procs = append(procs, p)
	}
	return procs, nil
}

func readCmdline(file string) ([]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxCmdline))
	if err != nil {
		return nil, err
	}
	return parseCmdline(b), nil
}

// parseCmdline splits a NUL-separated argv. An empty cmdline (a kernel
// thread, a zombie) gives no arguments.
func parseCmdline(b []byte) []string {
	b = bytes.TrimRight(b, "\x00")
	if len(b) == 0 {
		return nil
	}
	return strings.Split(string(b), "\x00")
}

// parseProcStat parses /proc/<pid>/stat: "pid (comm) state ppid pgrp session
// tty_nr tpgid ...". The command name may contain spaces and parentheses, so
// the fields start after the last ")".
func parseProcStat(b []byte) (Proc, bool) {
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return Proc{}, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 6 {
		return Proc{}, false
	}
	ppid, err1 := strconv.Atoi(f[1])
	pgrp, err2 := strconv.Atoi(f[2])
	ttyNr, err3 := strconv.Atoi(f[4])
	tpgid, err4 := strconv.Atoi(f[5])
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return Proc{}, false
	}
	tty := ""
	if ttyNr != 0 {
		tty = strconv.Itoa(ttyNr) // the device number: identifies the terminal
	}
	return Proc{PPID: ppid, PGRP: pgrp, TTY: tty, TPGID: tpgid, State: f[0]}, true
}

// PSSource reads the process table with ps(1), for macOS and the BSDs. It
// runs one short command per call and never logs or stores the command lines
// it reads. Where ps is missing or fails (or lacks a column), it returns an
// error and classification falls back to the current command.
type PSSource struct{}

const psTimeout = 2 * time.Second

// Processes implements ProcessSource.
func (PSSource) Processes(ctx context.Context) ([]Proc, error) {
	ctx, cancel := context.WithTimeout(ctx, psTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,pgid=,tpgid=,tty=,stat=,args=")
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	procs := parsePS(out.String())
	if len(procs) == 0 {
		return nil, errors.New("agentdetect: ps returned no processes")
	}
	return procs, nil
}

// parsePS parses "pid ppid pgid tpgid tty stat args..." lines; malformed
// lines are skipped. A process without a terminal ("?", "??", "-") has an
// empty TTY.
func parsePS(s string) []Proc {
	var procs []Proc
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 7 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		pgid, err3 := strconv.Atoi(f[2])
		tpgid, err4 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		procs = append(procs, Proc{PID: pid, PPID: ppid, PGRP: pgid, TPGID: tpgid, TTY: psTTY(f[4]), State: f[5], Args: f[6:]})
	}
	return procs
}

// psTTY normalises the tty column of ps: "?", "??" and "-" mean no terminal.
func psTTY(s string) string {
	switch s {
	case "?", "??", "-":
		return ""
	}
	return s
}
