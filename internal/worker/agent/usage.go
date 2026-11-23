package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
			scan, err := cache.get(ctx, grant, a.Fresh)
			if err != nil {
				return nil, err
			}
			r.Inventory, r.Accounts, r.InventoryAt = scan.Inventory, mergeUsageAccounts(scan.Accounts, r.Accounts), scan.InventoryAt
			r.ObservedAt = scan.ObservedAt
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
		if err := fitUsageReport(&r, time.Now()); err != nil {
			return nil, apiErr(proto.CodeConflict, "", "usage report exceeds a collection bound")
		}
		return r, nil
	})
}

// usageDetector classifies the panes of the usage snapshot (tests replace it).
var usageDetector = agentdetect.New()

// usageProcTimeout bounds the one process-table query of a usage snapshot
// (tests shorten it).
var usageProcTimeout = agentdetect.SnapshotTimeout

// usageHarnessProvider names the vendor of a harness that has exactly one.
var usageHarnessProvider = map[string]string{"claude": "Anthropic", "codex": "OpenAI"}

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
	accounts := map[string]ops.UsageAccount{}
	// One bounded process-table query serves every pane of this snapshot.
	detector := usageDetector.Snapshot(usageProcTimeout)
	for _, s := range sessions {
		var started *int64
		if !s.Created.IsZero() && !s.Created.After(now) {
			v := s.Created.UnixMilli()
			started = &v
		}
		row := ops.UsageSession{ID: ops.UsageID("host-session", s.Name, s.HostID, s.Created.UTC().Format(time.RFC3339Nano)), Name: s.Name, ObservedAt: at, StartedAt: started, AuthMode: "unknown", TimeSource: "worker_measured", Source: "session-host"}
		if pane, ok := s.ActivePane(); ok {
			row.Folder = pane.Folder
			// The classifier also finds an agent started by a wrapper
			// (bash -c 'claude ...; exec bash'), where the pane's current
			// command is the shell. Only the process tree and the command
			// are read, never the screen.
			if c, pid := detector.Locate(ctx, agentdetect.Input{Command: pane.Ref.Command, PID: pane.Ref.PID}); c.ShownIsAgent() {
				row.Harness = c.ShownAgent()
				row.Provider = usageHarnessProvider[row.Harness]
				acc := detectSessionAccess(row.Harness, pid)
				row.AuthMode, row.CustomEndpoint = acc.Mode, acc.Kind == accessCustomEndpoint
				if a, ok := accessAccount(row.Provider, acc, at); ok {
					row.AccountID = a.ID
					accounts[a.ID] = a
				}
			}
		}
		r.Sessions = append(r.Sessions, row)
	}
	for _, a := range accounts {
		r.Accounts = append(r.Accounts, a)
	}
	slices.SortFunc(r.Accounts, func(a, b ops.UsageAccount) int { return strings.Compare(a.ID, b.ID) })
	if inventory {
		r.Inventory = usageInventory(ctx)
		r.Accounts = mergeUsageAccounts(discoverUsageAccounts(ctx, r.Inventory, ""), r.Accounts)
		refreshClaudeExporter(r.Inventory)
		r.ObservedAt = time.Now().UnixMilli()
	}
	if err := fitUsageReport(&r, time.Now()); err != nil {
		return ops.UsageReport{}, apiErr(proto.CodeConflict, "", "session metadata could not be represented")
	}
	return r, nil
}

// maxUsageAccounts is the wire bound on the accounts of one report.
const maxUsageAccounts = 128

// fitUsageReport makes the report valid by dropping what is optional: an
// account that cannot be represented (or beyond the bound) is left out and a
// bad label is emptied, so that growth or one odd account never invalidates
// the sessions and the rest of the report. Session-derived accounts (few,
// grouped by access kind) come before discovered ones when the bound cuts.
func fitUsageReport(r *ops.UsageReport, now time.Time) error {
	*r = r.DropBadLabels()
	kept := make([]ops.UsageAccount, 0, len(r.Accounts))
	probe := ops.UsageReport{Version: r.Version, ObservedAt: r.ObservedAt}
	for _, a := range r.Accounts {
		probe.Accounts = []ops.UsageAccount{a}
		if probe.Check(now) == nil {
			kept = append(kept, a)
		}
	}
	slices.SortStableFunc(kept, func(a, b ops.UsageAccount) int {
		return b2i(b.Source == "session-host") - b2i(a.Source == "session-host")
	})
	if len(kept) > maxUsageAccounts {
		kept = kept[:maxUsageAccounts]
	}
	slices.SortStableFunc(kept, func(a, b ops.UsageAccount) int {
		if a.ID == b.ID {
			return strings.Compare(a.SourceSessionID, b.SourceSessionID)
		}
		return strings.Compare(a.ID, b.ID)
	})
	r.Accounts = kept
	return r.Check(now)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// mergeUsageAccounts keeps every account of both lists once per identity (id
// and source session): a discovered account wins over a session-derived one
// with the same identity, and the session-derived API accounts that no scan
// knows stay.
func mergeUsageAccounts(discovered, fromSessions []ops.UsageAccount) []ops.UsageAccount {
	out := slices.Clone(discovered)
	seen := map[string]bool{}
	for _, a := range out {
		seen[a.ID+"|"+a.SourceSessionID] = true
	}
	for _, a := range fromSessions {
		if !seen[a.ID+"|"+a.SourceSessionID] {
			out = append(out, a)
		}
	}
	return out
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
			probeInGroup(cmd)
			err := cmd.Run()
			endProbeGroup(cmd)
			// A helper the harness left running can hold the output open; the
			// harness itself exited cleanly and its output is complete.
			if err == nil || errors.Is(err, exec.ErrWaitDelay) {
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

func refreshClaudeExporter(inventory []ops.UsageHarness) {
	for i := range inventory {
		if inventory[i].Name == "claude" && inventory[i].Status == "installed" {
			inventory[i].Exporter = claudeExporterState()
			return
		}
	}
}

const usageForceInterval = 10 * time.Second

var usageInventoryGeneration atomic.Uint64

// InvalidateUsageInventory makes the next inventory request rescan after a
// local profile change. A scan already running belongs to the old generation.
func InvalidateUsageInventory() { usageInventoryGeneration.Add(1) }

type usageInventoryCache struct {
	mu         sync.Mutex
	running    chan struct{}
	grant      string
	report     ops.UsageReport
	until      time.Time
	generation uint64
	nextForced time.Time
}

func (c *usageInventoryCache) get(ctx context.Context, grant string, fresh bool) (ops.UsageReport, error) {
	for {
		c.mu.Lock()
		now := time.Now()
		generation := usageInventoryGeneration.Load()
		force := fresh && !now.Before(c.nextForced)
		done := c.running
		valid := c.grant == grant && c.generation == generation && now.Before(c.until)
		if valid && !force && (!fresh || done == nil) {
			out := c.report
			// Order reports by when they saw the cached inventory, before
			// reading the profile outside the lock.
			out.ObservedAt = now.UnixMilli()
			out.Inventory = slices.Clone(out.Inventory)
			out.Accounts = slices.Clone(out.Accounts)
			c.mu.Unlock()
			refreshClaudeExporter(out.Inventory)
			return out, nil
		}
		// A manual request can share a scan already in progress.
		fresh = false
		if done != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return ops.UsageReport{}, ctx.Err()
			case <-done:
				continue
			}
		}
		if force {
			c.nextForced = now.Add(usageForceInterval)
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
			c.grant, c.generation, c.until, c.report = grant, generation, at.Add(5*time.Minute), out
		}
		close(c.running)
		c.running = nil
		c.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return ops.UsageReport{}, err
		}
	}
}

var claudeExporterIssue atomic.Value // string

// SetClaudeExporterIssue records why the automatic exporter install did not
// happen ("off", "overridden", "conflict", or "" for none); the harness scan
// reports it.
func SetClaudeExporterIssue(state string) { claudeExporterIssue.Store(state) }

// ClaudeManagedSettingsFile is the machine-wide Claude Code settings file that
// outranks user settings (a variable so that tests can point it elsewhere).
var ClaudeManagedSettingsFile = claudeManagedSettingsPath()

// ClaudeExporterIssue returns the recorded install outcome (see
// SetClaudeExporterIssue).
func ClaudeExporterIssue() string { s, _ := claudeExporterIssue.Load().(string); return s }

func claudeManagedSettingsPath() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ClaudeCode/managed-settings.json"
	case "windows":
		programFiles := os.Getenv("ProgramFiles")
		if programFiles == "" {
			programFiles = `C:\Program Files`
		}
		return filepath.Join(programFiles, "ClaudeCode", "managed-settings.json")
	}
	return "/etc/claude-code/managed-settings.json"
}

// ClaudeManagedOverrides reports whether the managed settings at path (the
// file and the drop-in files of the managed-settings.d directory beside it,
// Claude Code's documented locations on each OS) define a status line or
// control hooks, which user settings cannot override. It fails closed: a
// managed file or directory that exists but cannot be read within bounds, is
// not a regular file or cannot be parsed counts as an override, because its
// policy is unknown. A pipe in its place never stalls the caller.
func ClaudeManagedOverrides(path string) bool {
	files := []string{path}
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(path), "managed-settings.d"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return true
	default:
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") && !e.IsDir() {
				files = append(files, filepath.Join(filepath.Dir(path), "managed-settings.d", e.Name()))
			}
		}
	}
	for _, f := range files {
		if _, err := os.Stat(f); errors.Is(err, os.ErrNotExist) {
			continue
		}
		raw, ok := readProfileFile(f)
		if !ok {
			return true
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return true
		}
		for _, k := range []string{"statusLine", "disableAllHooks", "allowManagedHooksOnly"} {
			if _, ok := m[k]; ok {
				return true
			}
		}
	}
	return false
}

// ClaudeProfileDirs returns the Claude Code profile directories this Worker
// knows: the default one and CLAUDE_CONFIG_DIR.
func ClaudeProfileDirs() []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".claude"))
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); filepath.IsAbs(d) && !slices.Contains(dirs, d) {
		dirs = append(dirs, d)
	}
	return dirs
}

// claudeExporterState reports the exporter state of the default Claude profile
// (CLAUDE_CONFIG_DIR or ~/.claude): installed, absent, overridden (hooks
// disabled or managed settings win), off or conflict. Only the state leaves
// the Worker.
func claudeExporterState() string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ops.UsageExporterAbsent
		}
		dir = filepath.Join(home, ".claude")
	}
	state := claudeExporterStateIn(dir)
	if state != ops.UsageExporterInstalled {
		if issue, _ := claudeExporterIssue.Load().(string); issue != "" {
			return issue
		}
	}
	return state
}
func claudeExporterStateIn(dir string) string {
	raw, ok := readProfileFile(filepath.Join(dir, "settings.json"))
	if !ok {
		return ops.UsageExporterAbsent
	}
	var s struct {
		StatusLine struct {
			Command string `json:"command"`
		} `json:"statusLine"`
		DisableAllHooks bool `json:"disableAllHooks"`
	}
	if json.Unmarshal(raw, &s) != nil || !strings.Contains(s.StatusLine.Command, "_claude-export") {
		return ops.UsageExporterAbsent
	}
	if s.DisableAllHooks || ClaudeManagedOverrides(ClaudeManagedSettingsFile) {
		return ops.UsageExporterOverridden
	}
	return ops.UsageExporterInstalled
}

// maxUsageLabel is the longest account label the Worker sends. It is the
// bound older Hubs (v0.19) accept; a longer login falls back to an opaque
// label instead of being cut.
const maxUsageLabel = 96

// accessAccount is the grouping row for sessions that use an API key or a
// custom endpoint: one per provider and access kind. It holds no key
// material, no host and no subscription limits.
func accessAccount(provider string, acc sessionAccess, at int64) (ops.UsageAccount, bool) {
	if acc.Mode != "api" || provider == "" {
		return ops.UsageAccount{}, false
	}
	// The Worker never reads a key, so keys of one provider cannot be told
	// apart: the row says so instead of posing as one credential.
	label, kind := "API keys (not told apart)", accessAPIKey
	if acc.Kind == accessCustomEndpoint {
		label, kind = "Custom endpoint (not told apart)", accessCustomEndpoint
	}
	return ops.UsageAccount{ID: ops.UsageID("session-access", provider, kind), Provider: provider, Label: label, CustomEndpoint: kind == accessCustomEndpoint, AuthMode: "api", Source: "session-host", ObservedAt: at, TimeSource: "worker_measured"}, true
}
