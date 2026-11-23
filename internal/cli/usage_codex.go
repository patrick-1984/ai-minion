package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/version"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/spf13/cobra"
)

var codexLifecycleHooks = []struct{ name, kind string }{
	{"SessionStart", "session_start"},
	{"UserPromptSubmit", "turn_start"},
	{"PreModelRequest", "turn_start"},
	{"Stop", "turn_end"},
	{"Interrupt", "turn_end"},
	{"SessionEnd", "session_end"},
}

const codexExportCommand = "_codex-export"

func codexExporterEnabled(res *config.Resolver) bool {
	if res == nil {
		return false
	}
	// A saved opt-out also applies to hooks already loaded in a Codex
	// process whose environment may predate the setting.
	if res.File != nil {
		if value, _, err := res.File.Lookup(config.KeyUsageCodexExporter); err != nil || value == "off" {
			return false
		}
	}
	value, err := res.Resolve(config.KeyUsageCodexExporter)
	return err == nil && value.Value == "auto"
}

func autoInstallCodexExporter(ctx context.Context, res *config.Resolver, stateDir string, lookup func(string) (string, bool)) bool {
	if !codexHookPlatform() || !codexExporterEnabled(res) || ctx.Err() != nil {
		return false
	}
	dir, _ := lookup("CODEX_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		dir = filepath.Join(home, ".codex")
	}
	if !filepath.IsAbs(dir) {
		return false
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return false
	}
	st, err := store.OpenContext(ctx, stateDir)
	if err != nil {
		return false
	}
	defer func() { _ = st.Close() }()
	if grant := codexUsageGrant(ctx, st); grant == "" {
		return ctx.Err() == nil
	}
	codex, err := exec.LookPath("codex")
	if err != nil || !filepath.IsAbs(codex) {
		return false
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probe, codex, "--version")
	cmd.Dir = filepath.Dir(codex)
	cmd.WaitDelay = 200 * time.Millisecond
	output, err := cmd.Output()
	if err != nil || !supportedCodexHooks(string(output)) {
		return false
	}
	binary, err := os.Executable()
	if err == nil {
		_ = installCodexHooks(ctx, dir, binary, stateDir)
	}
	return false
}

func codexHookPlatform() bool {
	return runtime.GOOS == "linux" || runtime.GOOS == "android" || runtime.GOOS == "darwin"
}

func supportedCodexHooks(output string) bool {
	release, ok := strings.CutPrefix(strings.TrimSpace(output), "codex-cli ")
	if !ok {
		return false
	}
	found, err := version.ParseVersion(release)
	return err == nil && found.Compare(version.SemVer{Minor: 171}) >= 0
}

func codexUsageGrant(ctx context.Context, st *store.Store) string {
	enrollment, err := st.LoadEnrollment(ctx)
	if err != nil {
		return ""
	}
	grant, err := st.UsageGrantID(ctx, enrollment.WorkerID)
	if err != nil {
		return ""
	}
	return grant
}

func installCodexHooks(ctx context.Context, dir, binary, stateDir string) error {
	path := filepath.Join(dir, "hooks.json")
	release, err := lockConfigFileContext(ctx, filepath.Join(dir, ".minion-codex-usage.lock"))
	if err != nil {
		return err
	}
	defer release()
	before, err := readClaudeUsageFile(path, false)
	if err != nil {
		return err
	}
	next, err := mergeCodexHooks(before, codexHookCommand(binary, stateDir))
	if err != nil || next == nil {
		return err
	}
	return identity.WriteReplacing(path, next, func() error {
		current, err := readClaudeUsageFile(path, false)
		if err != nil || !bytes.Equal(current, before) {
			return identity.ErrChangedDuringWrite
		}
		return ctx.Err()
	})
}

func codexHookCommand(binary, stateDir string) string {
	quote := func(word string) string {
		return "'" + strings.ReplaceAll(word, "'", "'\"'\"'") + "'"
	}
	return quote(binary) + " worker config usage integrate " + codexExportCommand + " " + quote(stateDir)
}

func mergeCodexHooks(raw []byte, command string) ([]byte, error) {
	root := make(map[string]json.RawMessage)
	hooks := make(map[string]json.RawMessage)
	if raw != nil {
		var err error
		root, err = claudeUsageObject(raw)
		if err != nil {
			return nil, err
		}
		if source, ok := root["hooks"]; ok {
			hooks, err = claudeUsageObject(source)
			if err != nil {
				return nil, err
			}
		}
	}
	changed := false
	for _, event := range codexLifecycleHooks {
		var groups []json.RawMessage
		if source, ok := hooks[event.name]; ok && (json.Unmarshal(source, &groups) != nil || groups == nil) {
			return nil, errors.New("unsupported Codex hook group list")
		}
		found := false
		for _, rawGroup := range groups {
			var group struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
				} `json:"hooks"`
			}
			if json.Unmarshal(rawGroup, &group) != nil || group.Hooks == nil {
				return nil, errors.New("unsupported Codex hook group")
			}
			for _, handler := range group.Hooks {
				if !strings.Contains(handler.Command, " integrate "+codexExportCommand+" ") {
					continue
				}
				if found || group.Matcher != "" || handler.Type != "command" || handler.Command != command {
					return nil, errors.New("a conflicting Minion hook exists in this Codex profile")
				}
				found = true
			}
		}
		if found {
			continue
		}
		group, err := json.Marshal(map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": command, "timeout": 2,
		}}})
		if err != nil {
			return nil, err
		}
		hooks[event.name], err = json.Marshal(append(groups, group))
		if err != nil {
			return nil, err
		}
		changed = true
	}
	if !changed {
		return nil, nil
	}
	var err error
	root["hooks"], err = json.Marshal(hooks)
	if err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if len(out) >= maxClaudeSettings {
		return nil, errors.New("the Codex hooks exceed the supported file size")
	}
	return append(out, '\n'), err
}

func parseCodexLifecycle(r io.Reader, grant string, at time.Time) (store.HarnessEvent, bool) {
	var input struct {
		Event   string `json:"hook_event_name"`
		Session string `json:"session_id"`
		Turn    string `json:"turn_id"`
		Source  string `json:"source"`
		AgentID string `json:"agent_id"`
	}
	// Only native IDs and event metadata survive decoding. No prompt,
	// transcript path, model request, or response is recorded or logged.
	const maxInput = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(r, maxInput+1))
	if err != nil || len(raw) > maxInput || json.Unmarshal(raw, &input) != nil || input.Session == "" || len(input.Session) > 128 {
		return store.HarnessEvent{}, false
	}
	for _, hook := range codexLifecycleHooks {
		if hook.name != input.Event {
			continue
		}
		if hook.kind == "session_start" && input.Source == "compact" {
			// A compaction can happen during an active turn.
			return store.HarnessEvent{}, false
		}
		if hook.name == "PreModelRequest" && input.AgentID != "" {
			// Codex can use the parent session ID on a child request.
			return store.HarnessEvent{}, false
		}
		event := store.HarnessEvent{
			Harness: "codex", Kind: hook.kind, At: at,
			NativeID: ops.UsageID("codex-thread", grant, input.Session),
		}
		if hook.kind == "turn_start" || hook.kind == "turn_end" {
			if input.Turn == "" || len(input.Turn) > 128 {
				return store.HarnessEvent{}, false
			}
			event.Entity = ops.UsageID("codex-turn", grant, input.Session, input.Turn)
		}
		return event, true
	}
	return store.HarnessEvent{}, false
}

func newCodexUsageExportCmd(lookup func(string) (string, bool)) *cobra.Command {
	return &cobra.Command{
		Use: codexExportCommand + " <state-dir>", Hidden: true, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			observedAt := time.Now()
			// Codex expects neutral JSON on stdout for Stop. Telemetry
			// failures must never block a prompt or continue a turn.
			defer func() { _, _ = io.WriteString(cmd.OutOrStdout(), "{}\n") }()
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Second)
			defer cancel()
			stateDir := args[0]
			if !filepath.IsAbs(stateDir) {
				return nil
			}
			cfg, err := config.LoadFile(filepath.Join(stateDir, config.WorkerConfigFile))
			if err != nil || !codexExporterEnabled(&config.Resolver{Role: config.ScopeWorker, File: cfg, LookupEnv: lookup}) {
				return nil
			}
			session, pane, created, pid, birth := harnessPaneProcess(ctx, lookup, stateDir, "codex")
			if session == "" || pane == "" || created <= 0 || pid <= 0 || birth == "" {
				return nil
			}
			st, err := store.OpenContext(ctx, stateDir)
			if err != nil {
				return nil
			}
			defer func() { _ = st.Close() }()
			if grant := codexUsageGrant(ctx, st); grant != "" {
				event, ok := parseCodexLifecycle(cmd.InOrStdin(), grant, observedAt)
				if ok {
					event.Session, event.Pane, event.Created = session, pane, created
					event.ProcessPID, event.ProcessBirth = pid, birth
					_ = st.RecordHarnessEvent(ctx, grant, event)
				}
			}
			return nil
		},
	}
}
