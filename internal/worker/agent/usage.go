package agent

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

type usageRegistration interface {
	UsageRegistration(context.Context, string) (bool, error)
}

// The narrow host interface makes a screen capture or keystroke impossible
// from the metrics projection.
type usageSessionHost interface {
	ListSessions(context.Context) ([]term.Session, error)
}

func (e *Executor) registerUsage(reg usageRegistration) {
	e.Handle(ops.OpWorkerUsage, func(ctx context.Context, rq Request) (any, error) {
		_, a, err := prepare[ops.WorkerUsageArgs](rq)
		if err != nil {
			return nil, err
		}
		if a.WorkerID == "" || a.WorkerID != e.policy.WorkerID() {
			return nil, apiErr(proto.CodeForbidden, "", "usage requires this registered Worker's id")
		}
		allowed, err := reg.UsageRegistration(ctx, a.WorkerID)
		if err != nil {
			return nil, internalErr("read usage registration", err)
		}
		if !allowed {
			return nil, apiErr(proto.CodeForbidden, "", "usage monitoring awaits local enrollment acceptance; run minion worker config usage on the Worker")
		}
		r, err := usageSnapshot(ctx, e.host, a.Inventory)
		if err != nil {
			return nil, err
		}
		// An owner change or reset while a slow inventory was running cancels it.
		allowed, err = reg.UsageRegistration(ctx, a.WorkerID)
		if err != nil {
			return nil, internalErr("check usage registration", err)
		}
		if !allowed {
			return nil, apiErr(proto.CodeForbidden, "", "the usage enrollment changed")
		}
		return r, nil
	})
}

func usageSnapshot(ctx context.Context, host usageSessionHost, inventory bool) (ops.UsageReport, error) {
	sessions, err := host.ListSessions(ctx)
	if err != nil {
		return ops.UsageReport{}, apiErr(proto.CodeConflict, "", "session metadata is unavailable")
	}
	now := time.Now().UTC()
	at := now.UnixMilli()
	r := ops.UsageReport{Version: ops.UsageVersion, ObservedAt: at, Complete: true, Sessions: []ops.UsageSession{}}
	if len(sessions) > 256 {
		return r, apiErr(proto.CodeConflict, "", "session inventory exceeds the supported batch size")
	}
	agents := agentdetect.DefaultAgents()
	for _, s := range sessions {
		var started *int64
		if !s.Created.IsZero() && !s.Created.After(now) {
			v := s.Created.UnixMilli()
			started = &v
		}
		row := ops.UsageSession{ID: ops.UsageID("host-session", s.Name, s.Created.UTC().Format(time.RFC3339Nano)), Name: s.Name, ObservedAt: at, StartedAt: started, AuthMode: "unknown", Source: "session-host"}
		if pane, ok := s.ActivePane(); ok {
			name := strings.TrimSuffix(strings.ToLower(filepath.Base(pane.Ref.Command)), ".exe")
			for _, known := range agents {
				for _, command := range known.Commands {
					if strings.EqualFold(name, command) {
						row.Harness = known.Name
					}
				}
			}
		}
		r.Sessions = append(r.Sessions, row)
	}
	if inventory {
		r.Inventory = usageInventory(ctx)
	}
	if err := r.Check(now); err != nil {
		return ops.UsageReport{}, apiErr(proto.CodeConflict, "", "session metadata could not be represented")
	}
	return r, nil
}

var usageVersionPattern = regexp.MustCompile(`(?:^|[^0-9])v?([0-9]{1,6}\.[0-9]{1,6}(?:\.[0-9]{1,8})?(?:-[0-9A-Za-z.-]{1,24})?)(?:$|[^0-9])`)

// The Worker executes only known version commands without a shell, in the
// installed executable's directory, with bounded time and output. It never
// sends stdout or a full command line to the Hub.
func usageInventory(ctx context.Context) []ops.UsageHarness {
	out := make([]ops.UsageHarness, 0, 8)
	for _, name := range []string{"claude", "codex", "gemini", "opencode", "aider", "qwen", "cline"} {
		item := ops.UsageHarness{Name: name, Status: "unavailable"}
		path, err := exec.LookPath(name)
		if err == nil && filepath.IsAbs(path) {
			item.Status = "installed"
			probe, cancel := context.WithTimeout(ctx, 2*time.Second)
			cmd := exec.CommandContext(probe, path, "--version")
			cmd.Dir = filepath.Dir(path)
			cmd.WaitDelay = 200 * time.Millisecond
			var buf usageOutput
			cmd.Stdout = &buf
			cmd.Stderr = io.Discard
			if err := cmd.Run(); err == nil {
				if match := usageVersionPattern.FindStringSubmatch(string(buf)); len(match) > 1 {
					item.Version = match[1]
				}
			}
			cancel()
		}
		out = append(out, item)
	}
	return out
}

type usageOutput []byte

func (b *usageOutput) Write(p []byte) (int, error) {
	const max = 4096
	if len(p) > max-len(*b) {
		return 0, errors.New("version output exceeds limit")
	}
	*b = append(*b, p...)
	return len(p), nil
}
