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
			scan, err := cache.get(ctx, grant)
			if err != nil {
				return nil, err
			}
			r.Inventory, r.Accounts, r.InventoryAt = scan.Inventory, mergeUsageAccounts(scan.Accounts, r.Accounts), scan.InventoryAt
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
		if err := r.Check(time.Now()); err != nil {
			return nil, apiErr(proto.CodeConflict, "", "usage report exceeds a collection bound")
		}
		return r, nil
	})
}

// usageDetector classifies the panes of the usage snapshot (tests replace it).
var usageDetector = agentdetect.New()

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
			if c, pid := usageDetector.Locate(ctx, agentdetect.Input{Command: pane.Ref.Command, PID: pane.Ref.PID}); c.IsAgent() {
				row.Harness = c.Agent()
				row.Provider = usageHarnessProvider[row.Harness]
				acc := detectSessionAccess(row.Harness, pid)
				row.AuthMode, row.Endpoint = acc.Mode, acc.Endpoint
				if a, ok := accessAccount(row, at); ok {
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
		r.ObservedAt = time.Now().UnixMilli()
	}
	if err := r.Check(time.Now()); err != nil {
		return ops.UsageReport{}, apiErr(proto.CodeConflict, "", "session metadata could not be represented")
	}
	return r, nil
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
			if name == "claude" {
				item.Exporter = claudeExporterState()
			}
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
		return filepath.Join(os.Getenv("ProgramFiles"), "ClaudeCode", "managed-settings.json")
	}
	return "/etc/claude-code/managed-settings.json"
}

// ClaudeManagedOverrides reports whether the managed settings at path (the
// file and the drop-in files of the managed-settings.d directory beside it)
// define a status line or control hooks, which user settings cannot override.
// It fails closed: a managed file that exists but cannot be read or parsed
// counts as an override, because its policy is unknown.
func ClaudeManagedOverrides(path string) bool {
	files := []string{path}
	if dropIns, err := filepath.Glob(filepath.Join(filepath.Dir(path), "managed-settings.d", "*.json")); err == nil {
		files = append(files, dropIns...)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
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
	f, err := os.Open(filepath.Join(dir, "settings.json"))
	if err != nil {
		return ops.UsageExporterAbsent
	}
	defer func() { _ = f.Close() }()
	var s struct {
		StatusLine struct {
			Command string `json:"command"`
		} `json:"statusLine"`
		DisableAllHooks bool `json:"disableAllHooks"`
	}
	if json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&s) != nil || !strings.Contains(s.StatusLine.Command, "_claude-export") {
		return ops.UsageExporterAbsent
	}
	if s.DisableAllHooks || ClaudeManagedOverrides(ClaudeManagedSettingsFile) {
		return ops.UsageExporterOverridden
	}
	return ops.UsageExporterInstalled
}

// maxUsageLabel is the longest account label the Worker sends: a full e-mail
// address of at most 254 bytes (RFC 5321) and nothing longer.
const maxUsageLabel = 254

// accessAccount is the grouping row for sessions that use an API key or a
// custom endpoint: one per provider, access kind and endpoint host. It holds
// no key material and no subscription limits.
func accessAccount(s ops.UsageSession, at int64) (ops.UsageAccount, bool) {
	if s.AuthMode != "api" || s.Provider == "" {
		return ops.UsageAccount{}, false
	}
	// The Worker never reads a key, so keys of one endpoint cannot be told
	// apart: the row says so instead of posing as one credential.
	label := "API keys (default endpoint, not told apart)"
	if s.Endpoint != "" {
		label = "API keys at " + s.Endpoint + " (not told apart)"
	}
	if len(label) > maxUsageLabel {
		return ops.UsageAccount{}, false
	}
	return ops.UsageAccount{ID: ops.UsageID("session-access", s.Provider, s.Endpoint), Provider: s.Provider, Label: label, AuthMode: "api", Source: "session-host", ObservedAt: at, TimeSource: "worker_measured"}, true
}
