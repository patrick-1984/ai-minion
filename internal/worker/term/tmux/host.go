package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

const (
	// MinMajor and MinMinor are the oldest supported tmux version (3.2).
	MinMajor = 3
	MinMinor = 2

	defaultTimeout = 10 * time.Second
	maxStdout      = 16 << 20 // a capture of a very long scrollback stays far below this
	maxStderr      = 64 << 10
)

// ExecFunc runs one tmux invocation. args are the complete argument list after
// the executable name (so they start with -S <socket>); stdin may be nil. It
// returns the captured stdout and stderr, and a non-nil error when tmux could
// not be run or exited with a failure status. Tests replace it with a recorder;
// the default implementation runs the tmux executable.
type ExecFunc func(ctx context.Context, args []string, stdin []byte) (stdout, stderr []byte, err error)

// Option configures New.
type Option func(*Host)

// WithExec replaces the process runner. The version check in New goes through
// it as well.
func WithExec(f ExecFunc) Option { return func(h *Host) { h.exec = f } }

// WithBinary sets the tmux executable. The default is "tmux" from PATH.
func WithBinary(path string) Option { return func(h *Host) { h.bin = path } }

// WithTimeout bounds every single tmux invocation. The default is 10 seconds.
func WithTimeout(d time.Duration) Option { return func(h *Host) { h.timeout = d } }

// WithLogger sets the logger of the debug lines (version check, session
// listings). The default writes nothing.
func WithLogger(l *slog.Logger) Option { return func(h *Host) { h.SetLogger(l) } }

// SetLogger sets the logger of the debug lines; nil writes nothing. It never
// logs terminal content: commands, exit status, counts and errors only.
func (h *Host) SetLogger(l *slog.Logger) { h.log.Store(l) }

func (h *Host) logger() *slog.Logger {
	if l := h.log.Load(); l != nil {
		return l
	}
	return slog.New(slog.DiscardHandler)
}

// Host is the tmux implementation of term.SessionHost for one tmux server.
// It is safe for concurrent use: its only mutable state is the logger and the
// summary of the last listing, both safe for concurrent use.
type Host struct {
	socket  string
	bin     string
	timeout time.Duration
	exec    ExecFunc
	version Version
	// psmux drives psmux (Windows) instead of tmux: one server per session,
	// panes numbered per session, so every pane is addressed through its
	// session (paneTarget); an empty socket means the user's default
	// namespace (~\.psmux), where the user's own sessions are.
	psmux bool
	// log receives the debug lines; listDedup keeps a listing that repeats
	// the previous result out of the log.
	log       atomic.Pointer[slog.Logger]
	listDedup logx.Dedup
}

// MinPsmuxMajor and MinPsmuxMinor are the oldest supported psmux (3.4): older
// releases ignore -S and would drive the user's default server.
const (
	MinPsmuxMajor = 3
	MinPsmuxMinor = 4
)

// WithPsmux drives psmux instead of tmux (see Host.psmux).
func WithPsmux() Option { return func(h *Host) { h.psmux = true } }

// Psmux reports whether the host drives psmux.
func (h *Host) Psmux() bool { return h.psmux }

// paneTarget is the -t value of a pane: its Target when the host set one,
// else its id. A psmux pane without a session-qualified target is refused: a
// bare id names a pane of whichever session psmux used last.
func (h *Host) paneTarget(p term.PaneRef) (string, error) {
	if p.Target != "" {
		return p.Target, nil
	}
	if h.psmux {
		return "", fmt.Errorf("pane %s: psmux needs the session of a pane: %w", truncate(p.ID, 40), ops.ErrInvalidArgument)
	}
	return p.ID, nil
}

// New returns a Host for the tmux server on socketPath. The path must be
// absolute: config resolves it once from --tmux-socket or the user's default
// socket (AD-13). New runs "tmux -S <socket> -V" and refuses a tmux older than
// 3.2. It does not start a tmux server.
func New(socketPath string, opts ...Option) (*Host, error) {
	h := &Host{bin: "tmux", timeout: defaultTimeout}
	for _, o := range opts {
		o(h)
	}
	switch {
	case socketPath == "" && h.psmux:
		// psmux: the user's default namespace.
	case socketPath == "":
		return nil, errors.New("tmux: socket path is empty; an absolute path is required")
	case !filepath.IsAbs(socketPath):
		return nil, fmt.Errorf("tmux: socket path %q is not absolute; an absolute path is required", socketPath)
	default:
		h.socket = filepath.Clean(socketPath)
	}
	if h.exec == nil {
		h.exec = h.execProcess
	}
	out, err := h.run(context.Background(), "-V")
	h.logger().Debug("terminal host version check", "binary", h.bin, "psmux", h.psmux, "socket", h.socket,
		"output", truncate(strings.TrimSpace(string(out)), 120), "error", errText(err))
	if err != nil {
		return nil, fmt.Errorf("tmux: cannot determine the tmux version: %w", err)
	}
	v, err := ParseVersion(string(out))
	if err != nil {
		return nil, err
	}
	if h.psmux {
		pv, ok := psmuxVersion(string(out))
		if !ok {
			return nil, fmt.Errorf("tmux: %s is not psmux (no psmux version in %q)", h.bin, truncate(string(out), 80))
		}
		if !pv.AtLeast(MinPsmuxMajor, MinPsmuxMinor) {
			return nil, fmt.Errorf("tmux: psmux %s is too old; psmux %d.%d or newer is required", pv, MinPsmuxMajor, MinPsmuxMinor)
		}
	}
	if !v.AtLeast(MinMajor, MinMinor) {
		return nil, fmt.Errorf("tmux: version %s is too old; tmux %d.%d or newer is required", v, MinMajor, MinMinor)
	}
	h.version = v
	h.logger().Debug("terminal host ready", "binary", h.bin, "psmux", h.psmux, "version", v.String())
	return h, nil
}

// errText is err's text, or "" for nil (for a log attribute).
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Socket returns the socket path every invocation uses.
func (h *Host) Socket() string { return h.socket }

// Version returns the tmux version found by New.
func (h *Host) Version() Version { return h.version }

// Version is a parsed tmux version. Dev builds ("master", "next-3.6") parse as
// their number, or as new enough when they carry none.
type Version struct {
	Major, Minor int
	Suffix       string // trailing letter such as the "a" of 3.5a
	raw          string
	dev          bool
}

// AtLeast reports whether v is at least major.minor.
func (v Version) AtLeast(major, minor int) bool {
	if v.dev {
		return true
	}
	return v.Major > major || (v.Major == major && v.Minor >= minor)
}

// String returns the version as tmux printed it, without the "tmux " prefix.
func (v Version) String() string { return v.raw }

var versionRE = regexp.MustCompile(`(\d+)\.(\d+)([a-z]?)`)

// ParseVersion parses the output of "tmux -V", for example "tmux 3.5a",
// "tmux next-3.6" or "tmux master".
func ParseVersion(out string) (Version, error) {
	s := strings.TrimSpace(out)
	rest, ok := strings.CutPrefix(s, "tmux ")
	if !ok {
		return Version{}, fmt.Errorf("tmux: unexpected version output %q", truncate(s, 80))
	}
	rest = strings.TrimSpace(rest)
	if rest == "master" {
		return Version{raw: rest, dev: true}, nil
	}
	m := versionRE.FindStringSubmatch(rest)
	if m == nil {
		return Version{}, fmt.Errorf("tmux: cannot parse the version %q", truncate(rest, 80))
	}
	maj, err1 := strconv.Atoi(m[1])
	minor, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return Version{}, fmt.Errorf("tmux: cannot parse the version %q", truncate(rest, 80))
	}
	return Version{Major: maj, Minor: minor, Suffix: m[3], raw: rest}, nil
}

// run executes tmux with args after "-S <socket> -u". Every invocation of the
// adapter goes through run or runStdin, so no command can reach another server.
// -u makes tmux print UTF-8 regardless of the locale of the Worker process.
func (h *Host) run(ctx context.Context, args ...string) ([]byte, error) {
	return h.runStdin(ctx, nil, args...)
}

// runStdin is run with data on the standard input of tmux (for load-buffer,
// Story 2.5). Callers never put text content on argv.
func (h *Host) runStdin(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	stdout, err := h.runKeepOutput(ctx, stdin, args...)
	if err != nil {
		return nil, err
	}
	return stdout, nil
}

// runKeepOutput is runStdin for an invocation whose output says what happened
// even when it failed: a sequence of commands in which a later command (or a
// user hook that one of them triggers) fails has run its first commands, and
// their output is returned with the error.
func (h *Host) runKeepOutput(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	full := make([]string, 0, len(args)+3)
	if h.socket != "" {
		full = append(full, "-S", h.socket)
	}
	full = append(full, "-u")
	full = append(full, args...)
	stdout, stderr, err := h.exec(ctx, full, stdin)
	if err != nil {
		return stdout, newError(args, stderr, err)
	}
	return stdout, nil
}

// execProcess is the default ExecFunc: it runs the tmux executable with
// TMUX and TMUX_PANE removed from the environment.
func (h *Host) execProcess(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, args...)
	cmd.Env = cleanEnv(os.Environ())
	cmd.WaitDelay = time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out := &cappedBuffer{limit: maxStdout}
	errBuf := &cappedBuffer{limit: maxStderr}
	cmd.Stdout, cmd.Stderr = out, errBuf
	err := cmd.Run()
	if err == nil && out.exceeded {
		err = fmt.Errorf("output exceeds %d bytes", maxStdout)
	}
	return out.Bytes(), errBuf.Bytes(), err
}

func cleanEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if k == "TMUX" || k == "TMUX_PANE" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// cappedBuffer keeps at most limit bytes and records that more was written.
// It always reports the full write so the child is not killed by a short write.
// The buffer is a named field, not embedded: embedding would promote
// bytes.Buffer.ReadFrom, which os/exec's output copy prefers over Write, and
// that would bypass the limit.
type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
}

func (c *cappedBuffer) Bytes() []byte  { return c.buf.Bytes() }
func (c *cappedBuffer) String() string { return c.buf.String() }
func (c *cappedBuffer) Len() int       { return c.buf.Len() }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room < len(p) {
		c.exceeded = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

// tmuxError is a failed tmux invocation. Its message names the tmux
// subcommand and tmux's own stderr text, never the full argument list.
type tmuxError struct {
	cmd    string
	stderr string
	err    error
}

func newError(args []string, stderr []byte, err error) error {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	return &tmuxError{cmd: cmd, stderr: strings.TrimSpace(truncate(string(stderr), 300)), err: err}
}

func (e *tmuxError) Error() string {
	if e.stderr != "" {
		return fmt.Sprintf("tmux %s: %s", e.cmd, e.stderr)
	}
	return fmt.Sprintf("tmux %s: %v", e.cmd, e.err)
}

func (e *tmuxError) Unwrap() error { return e.err }

// Is maps tmux's messages for a missing server, session, window or pane to
// term.ErrNotFound.
func (e *tmuxError) Is(target error) bool {
	return target == term.ErrNotFound && isNotFound(e.stderr)
}

// notFoundMarkers are the stderr texts tmux prints for a missing target and
// for a missing or dead server (an absent server has no sessions).
var notFoundMarkers = []string{
	"can't find session",
	"can't find window",
	"can't find pane",
	"no server running",
	"no current session",
	// A running server without sessions (exit-empty off, or the moment the
	// last session ended) answers list-panes -a with this.
	"no current target",
	"(No such file or directory)",
	"(Connection refused)",
}

func isNotFound(stderr string) bool {
	for _, m := range notFoundMarkers {
		if strings.Contains(stderr, m) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// psmuxVersion reads the "psmux X.Y.Z" line of psmux -V (which prints the
// tmux version it is compatible with first).
func psmuxVersion(out string) (Version, bool) {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "psmux "); ok {
			f := strings.Fields(rest)
			if len(f) == 0 {
				return Version{}, false
			}
			v, err := ParseVersion("tmux " + f[0])
			return v, err == nil
		}
	}
	return Version{}, false
}
