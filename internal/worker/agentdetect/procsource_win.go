package agentdetect

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// WinSource reads the process table on Windows with one PowerShell query
// (process id, parent id and command line). Windows has no process groups or
// controlling terminals, so the foreground process cannot be told: every
// process is marked UnknownForeground. The tree below the pane's shell is
// telemetry evidence only (usage metadata); it never authorizes input, so an
// agent in the background below PowerShell cannot make its shell writable.
type WinSource struct{}

const winPSTimeout = 5 * time.Second

const winPSScript = `Get-CimInstance Win32_Process | ForEach-Object { '{0}|{1}|{2}' -f $_.ProcessId, $_.ParentProcessId, $_.CommandLine }`

// Processes implements ProcessSource.
func (WinSource) Processes(ctx context.Context) ([]Proc, error) {
	ctx, cancel := context.WithTimeout(ctx, winPSTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", winPSScript)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	procs := parseWinProcs(out.String())
	if len(procs) == 0 {
		return nil, errors.New("agentdetect: no processes listed")
	}
	return procs, nil
}

// parseWinProcs parses "pid|ppid|command line" lines; malformed lines are
// skipped.
func parseWinProcs(s string) []Proc {
	var procs []Proc
	for _, line := range strings.Split(s, "\n") {
		parts := strings.SplitN(strings.TrimRight(line, "\r"), "|", 3)
		if len(parts) != 3 {
			continue
		}
		pid, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		ppid, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		args := splitWinCommandLine(parts[2])
		if err1 != nil || err2 != nil || pid <= 0 || len(args) == 0 {
			continue
		}
		procs = append(procs, Proc{PID: pid, PPID: ppid, UnknownForeground: true, Args: args})
	}
	return procs
}

// splitWinCommandLine splits a Windows command line on spaces outside double
// quotes and removes the quotes.
func splitWinCommandLine(s string) []string {
	var args []string
	var cur strings.Builder
	quoted, has := false, false
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r == '"':
			quoted, has = !quoted, true
		case (r == ' ' || r == '\t') && !quoted:
			if has {
				args = append(args, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if has {
		args = append(args, cur.String())
	}
	return args
}
