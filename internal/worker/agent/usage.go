package agent

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

type usageRegistration interface {
	UsageGrantID(context.Context, string) (string, error)
	NativeUsage(context.Context, *ops.UsageReport, int64) error
}

// The narrow host interface makes a screen capture or keystroke impossible
// from the metrics projection.
type usageSessionHost interface {
	ListSessions(context.Context) ([]term.Session, error)
}

func (e *Executor) registerUsage(reg usageRegistration) {
	var cache usageInventoryCache
	e.Handle(ops.OpWorkerUsage, func(ctx context.Context, rq Request) (any, error) {
		_, a, err := prepare[ops.WorkerUsageArgs](rq)
		if err != nil {
			return nil, err
		}
		if a.WorkerID == "" || a.WorkerID != e.policy.WorkerID() {
			return nil, apiErr(proto.CodeForbidden, "", "usage requires this registered Worker's id")
		}
		grant, err := reg.UsageGrantID(ctx, a.WorkerID)
		if err != nil {
			return nil, internalErr("read usage registration", err)
		}
		if grant == "" {
			return nil, apiErr(proto.CodeForbidden, "", "usage monitoring awaits local enrollment acceptance; run minion worker config usage on the Worker")
		}
		var r ops.UsageReport
		if a.AccountID != "" {
			if rq.Origin == nil || rq.Origin.Kind != proto.OriginWorker || rq.Origin.SourceWorkerID != a.WorkerID || rq.Origin.Grant {
				return nil, apiErr(proto.CodeForbidden, "", "only the Hub account collector can request a native limit refresh")
			}
			probe, stop := context.WithTimeout(ctx, 8*time.Second)
			account, queryErr := codexAccount(probe, a.AccountID)
			stop()
			r = ops.UsageReport{Version: ops.UsageVersion, ObservedAt: time.Now().UnixMilli(), Probe: &ops.UsageProbe{Status: "ready"}}
			if queryErr == nil {
				r.Accounts = []ops.UsageAccount{account}
			} else {
				r.Probe.Status = "unavailable"
				var failure nativeFailure
				if errors.As(queryErr, &failure) {
					r.Probe.Status = failure.status
					r.Probe.RetryAfterSeconds = failure.retryAfter
				}
			}
		} else {
			r, err = usageSnapshot(ctx, e.host, false)
			if err != nil {
				return nil, err
			}
		}
		if a.Inventory && a.AccountID == "" {
			scan, err := cache.get(ctx, grant)
			if err != nil {
				return nil, err
			}
			r.Inventory, r.Accounts, r.InventoryAt = scan.Inventory, scan.Accounts, scan.InventoryAt
			r.ObservedAt = time.Now().UnixMilli()
		}
		if a.AccountID == "" {
			if err := reg.NativeUsage(ctx, &r, a.AfterEvent); err != nil {
				return nil, internalErr("read local usage metadata", err)
			}
		}
		// An owner change or reset while a slow inventory was running cancels it.
		current, err := reg.UsageGrantID(ctx, a.WorkerID)
		if err != nil {
			return nil, internalErr("check usage registration", err)
		}
		if current == "" || current != grant {
			return nil, apiErr(proto.CodeForbidden, "", "the usage enrollment changed")
		}
		return r, nil
	})
}

func usageSnapshot(ctx context.Context, host usageSessionHost, inventory bool) (ops.UsageReport, error) {
	list := host.ListSessions
	if h, ok := host.(interface {
		ListUsageSessions(context.Context) ([]term.Session, error)
	}); ok {
		list = h.ListUsageSessions
	}
	sessions, err := list(ctx)
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
		row := ops.UsageSession{ID: ops.UsageID("host-session", s.Name, s.HostID, s.Created.UTC().Format(time.RFC3339Nano)), Name: s.Name, ObservedAt: at, StartedAt: started, AuthMode: "unknown", Source: "session-host"}
		if pane, ok := s.ActivePane(); ok {
			row.Folder = pane.Folder
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
		r.Accounts = discoverUsageAccounts(ctx, r.Inventory, "")
		r.ObservedAt = time.Now().UnixMilli()
	}
	if err := r.Check(time.Now()); err != nil {
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

type usageInventoryCache struct {
	mu      sync.Mutex
	running chan struct{}
	grant   string
	report  ops.UsageReport
	until   time.Time
}

func (c *usageInventoryCache) get(ctx context.Context, grant string) (ops.UsageReport, error) {
	for {
		c.mu.Lock()
		if c.grant == grant && time.Now().Before(c.until) {
			out := c.report
			out.Inventory = slices.Clone(out.Inventory)
			out.Accounts = slices.Clone(out.Accounts)
			c.mu.Unlock()
			return out, nil
		}
		if done := c.running; done != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return ops.UsageReport{}, ctx.Err()
			case <-done:
				continue
			}
		}
		c.running = make(chan struct{})
		c.mu.Unlock()
		harnesses := usageInventory(ctx)
		hubPublic, _, _ := strings.Cut(grant, ":")
		accounts := discoverUsageAccounts(ctx, harnesses, hubPublic)
		at := time.Now()
		ms := at.UnixMilli()
		out := ops.UsageReport{Inventory: harnesses, Accounts: accounts, InventoryAt: &ms}
		c.mu.Lock()
		if ctx.Err() == nil {
			c.grant, c.until, c.report = grant, at.Add(5*time.Minute), out
		}
		close(c.running)
		c.running = nil
		c.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return ops.UsageReport{}, err
		}
	}
}
