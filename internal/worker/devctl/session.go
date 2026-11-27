package devctl

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
)

// maxAncestry bounds the walk from a caller up to its pane.
const maxAncestry = 64

// Caller is who asks: the peer pid of the control socket connection, read by
// the Worker from the socket (never sent by the caller), and the Hub user when
// the Worker knows one verified for the session.
type Caller struct {
	PID   int
	Actor string
}

type callerKey struct{}

// WithCaller attaches the caller of a local request to ctx.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom returns the caller attached by [WithCaller].
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// MapSession maps a caller pid to the session whose pane process is pid or one
// of its ancestors, by walking the parent chain with ppid. panes maps a pane
// pid to its session. It answers [SessionUnknown] when the chain reaches no
// pane (a process outside tmux, a pid the Worker cannot read).
func MapSession(pid int, ppid func(int) (int, error), panes map[int]string) string {
	seen := map[int]bool{}
	for i := 0; pid > 1 && i < maxAncestry && !seen[pid]; i++ {
		if s, ok := panes[pid]; ok && s != "" {
			return s
		}
		seen[pid] = true
		next, err := ppid(pid)
		if err != nil {
			break
		}
		pid = next
	}
	return SessionUnknown
}

// ProcPPID reads the parent pid of pid from /proc/<pid>/stat. The second field
// (the command) is in parentheses and can hold spaces and parentheses, so the
// parent is read after the last ')'.
func ProcPPID(pid int) (int, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	return parseStatPPID(b)
}

func parseStatPPID(b []byte) (int, error) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, fmt.Errorf("devctl: unreadable stat")
	}
	f := bytes.Fields(b[i+1:])
	if len(f) < 2 {
		return 0, fmt.Errorf("devctl: unreadable stat")
	}
	return strconv.Atoi(string(f[1]))
}
