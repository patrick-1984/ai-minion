package filexfer

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The files probe (BRD 5.3): before a Worker (or a REST CLI) resolves a path
// with openat2 or statx in its own process, it runs its own binary once as a
// child ("minion worker files-probe", which calls ProbeChild) in a scratch
// folder with a symlink fixture. A seccomp filter that traps openat2 or statx
// (possible under the Android app seccomp policy) kills the child, not the
// Worker. The child reports each check on its own line before and after the
// call, so the parent knows which call killed it.
//
// Result: openat2 works (and refuses a symlink and "..") -> ResolveOpenat2;
// else statx reports mount ids -> ResolveWalk; else ResolveUnavailable, and
// the Worker does not announce files.v1 and shows Reason in "minion worker
// status". On macOS the probe needs no child: ResolveWalk. On Windows:
// ResolveUnavailable.

// ProbeCommand is the command that runs ProbeChild: the Worker's own
// executable with the arguments of the files-probe subcommand.
type ProbeCommand struct {
	Path string
	Args []string
	// Env is the child's environment; nil inherits the process environment.
	Env []string
}

// ProbeResult is the outcome of the probe.
type ProbeResult struct {
	// Mode is ResolveOpenat2, ResolveWalk or ResolveUnavailable.
	Mode ResolveMode
	// Openat2 and MountID tell what the probe saw: "ok", "failed: <why>",
	// "killed" (the call ended the child), or "" (not tried).
	Openat2 string
	MountID string
	// Reason explains ResolveUnavailable in plain English.
	Reason string
}

// Probe lines of the child.
const (
	probeBegin = "begin "
	probeOK    = " ok"
	probeFail  = " failed: "
	probeEnd   = "end"
)

// maxProbeOutput bounds what the parent reads from the child.
const maxProbeOutput = 4 << 10

// Probe runs the probe child c in a fresh folder created in scratch ("" uses
// the system temporary folder) and returns the resolution mode the process
// may use. It never calls openat2 or statx itself.
func Probe(ctx context.Context, c ProbeCommand, scratch string) ProbeResult {
	if r, done := probeHost(); done {
		return r
	}
	dir, err := os.MkdirTemp(scratch, "minion-files-probe-")
	if err != nil {
		return unavailable("the probe folder cannot be created")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := writeProbeFixture(dir); err != nil {
		return unavailable("the probe folder cannot be prepared")
	}
	cmd := exec.CommandContext(ctx, c.Path, c.Args...) //nolint:gosec // the Worker's own executable
	cmd.Env = c.Env
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, n: maxProbeOutput}
	cmd.Stderr = io.Discard
	runErr := cmd.Run()
	return interpretProbe(out.String(), runErr)
}

// writeProbeFixture creates d/f and the symlink l -> d.
func writeProbeFixture(dir string) error {
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "d", "f"), []byte("probe\n"), 0o600); err != nil {
		return err
	}
	return os.Symlink("d", filepath.Join(dir, "l"))
}

func unavailable(reason string) ProbeResult {
	return ProbeResult{Mode: ResolveUnavailable, Reason: reason}
}

// interpretProbe reads the child's lines.
func interpretProbe(out string, runErr error) ProbeResult {
	seen := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if name, ok := strings.CutPrefix(line, probeBegin); ok {
			if _, dup := seen[name]; !dup {
				seen[name] = "killed"
			}
			continue
		}
		for _, name := range []string{"statx", "openat2"} {
			if line == name+probeOK {
				seen[name] = "ok"
			} else if why, ok := strings.CutPrefix(line, name+probeFail); ok {
				seen[name] = "failed: " + why
			}
		}
	}
	r := ProbeResult{Openat2: seen["openat2"], MountID: seen["statx"]}
	switch {
	case r.MountID == "":
		if runErr != nil {
			r.Reason = "the files probe could not run: " + runErr.Error()
		} else {
			r.Reason = "the files probe reported nothing"
		}
		r.Mode = ResolveUnavailable
	case r.Openat2 == "ok":
		r.Mode = ResolveOpenat2
	case r.MountID == "ok":
		r.Mode = ResolveWalk
	default:
		r.Mode = ResolveUnavailable
		r.Reason = "the kernel offers neither openat2 nor mount ids (statx), so folders cannot be confined (openat2 " +
			orNotTried(r.Openat2) + ", statx " + r.MountID + ")"
	}
	return r
}

func orNotTried(s string) string {
	if s == "" {
		return "not tried"
	}
	return s
}

// ApplyProbe sets the process default resolution mode from r.
func ApplyProbe(r ProbeResult) { SetDefaultResolveMode(r.Mode) }

// ProbeChild runs the checks of the probe in the current folder (prepared by
// Probe) and writes one line per step to w. It is the body of the files-probe
// subcommand; it returns the exit code. A seccomp trap may end the process
// inside a check, which is the point.
func ProbeChild(w io.Writer) int {
	probeChildChecks(w)
	_, _ = fmt.Fprintln(w, probeEnd)
	return 0
}

// limitedWriter keeps the first n bytes and discards the rest.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
