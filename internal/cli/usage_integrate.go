package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/spf13/cobra"
)

const (
	claudeUsageManifest = "minion-usage.json"
	claudeUsageMarker   = "_claude-export"
	maxClaudeSettings   = 1 << 20
)

var claudeUsageEvents = []string{
	"SessionStart", "SessionEnd", "UserPromptSubmit", "Stop", "SubagentStart", "SubagentStop", "TaskCreated", "TaskCompleted", "CwdChanged", "PostModelSwitch",
}

type claudeUsageIntegration struct {
	Version      int    `json:"version"`
	Binary       string `json:"binary"`
	StateDir     string `json:"state_dir"`
	Grant        string `json:"grant"`
	Shell        string `json:"shell"`
	PowerShell   bool   `json:"powershell,omitempty"`
	PreviousLine string `json:"previous_status_line,omitempty"`
	Account      string `json:"account,omitempty"`
	Plan         string `json:"plan,omitempty"`
	Profile      string `json:"subscription_profile,omitempty"`
}

type claudeUsageHook struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Async   bool     `json:"async,omitempty"`
}

func newUsageIntegrateCmd(deps workerConfigDeps) *cobra.Command {
	cmd := &cobra.Command{Use: "integrate", Short: "Install a native usage exporter for a Worker profile"}
	var profileDir, account, profile string
	claude := &cobra.Command{
		Use: "claude", Short: "Enable Claude Code status-line and lifecycle usage for one local profile",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateClaudeUsageAttribution(account, profile); err != nil {
				return err
			}
			target, err := claudeUsageProfileDir(profileDir, deps.lookupEnv)
			if err != nil {
				return err
			}
			ce, err := deps.env(cmd)
			if err != nil {
				return err
			}
			defer ce.release()
			binary, err := os.Executable()
			if err != nil {
				return errors.New("the Minion executable path is unavailable")
			}
			shell, powerShell, err := claudeUsageShell(deps.lookupEnv)
			if err != nil {
				return err
			}
			integration := claudeUsageIntegration{
				Version: 1, Binary: binary, StateDir: ce.stateDir, Shell: shell, PowerShell: powerShell,
				Account: account, Profile: profile,
			}
			changed, err := installClaudeUsage(cmd.Context(), target, integration, agent.CurrentClaudeSubscription)
			if err != nil {
				return err
			}
			if !changed {
				return writef(ce.out, "Claude Code usage integration is already configured in %s.\n", target)
			}
			return writef(ce.out, "Claude Code usage integration is configured in %s. It applies when Claude Code loads this profile.\n", target)
		},
	}
	addNamedSettingFlags(claude, deps.reg(), config.KeyStateDir)
	claude.Flags().StringVar(&profileDir, "claude-config-dir", "", "Claude Code profile directory (defaults to CLAUDE_CONFIG_DIR or ~/.claude)")
	claude.Flags().StringVar(&account, "account", "", "Known Anthropic subscription account fingerprint from Minion Usage")
	claude.Flags().StringVar(&profile, "subscription-profile", "", "Explicit name shared across Workers using the same Anthropic subscription")
	cmd.AddCommand(claude, newClaudeUsageExportCmd(deps.lookupEnv))
	return cmd
}

func validateClaudeUsageAttribution(account, profile string) error {
	if account != "" {
		if _, err := hex.DecodeString(account); err != nil || len(account) != 64 || account != strings.ToLower(account) || profile != "" {
			return errors.New("use one lowercase account fingerprint or a subscription profile")
		}
	}
	if len(profile) > 80 || strings.IndexFunc(profile, unicode.IsControl) >= 0 || profile != "" && strings.TrimSpace(profile) == "" {
		return errors.New("use a nonempty subscription profile name of at most 80 bytes")
	}
	return nil
}

func claudeUsageProfileDir(explicit string, lookup func(string) (string, bool)) (string, error) {
	if explicit == "" && lookup != nil {
		explicit, _ = lookup("CLAUDE_CONFIG_DIR")
	}
	if explicit == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("give --claude-config-dir to select a Claude Code profile")
		}
		explicit = filepath.Join(home, ".claude")
	}
	abs, err := filepath.Abs(explicit)
	if err != nil || strings.IndexFunc(abs, unicode.IsControl) >= 0 {
		return "", errors.New("invalid Claude Code profile path")
	}
	return abs, nil
}

func claudeUsageShell(lookup func(string) (string, bool)) (string, bool, error) {
	if runtime.GOOS != "windows" {
		return "/bin/sh", false, nil
	}
	if lookup != nil {
		if shell, ok := lookup("CLAUDE_CODE_GIT_BASH_PATH"); ok && filepath.IsAbs(shell) {
			f, err := os.Stat(shell)
			if err == nil && f.Mode().IsRegular() {
				return shell, false, nil
			}
			return "", false, errors.New("the configured Claude Code Git Bash executable is unavailable")
		}
		for _, key := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			base, _ := lookup(key)
			if !filepath.IsAbs(base) {
				continue
			}
			if key == "LocalAppData" {
				base = filepath.Join(base, "Programs")
			}
			git := filepath.Join(base, "Git", "bin", "bash.exe")
			if stat, err := os.Stat(git); err == nil && stat.Mode().IsRegular() {
				return git, false, nil
			}
		}
	}
	path, err := exec.LookPath("powershell.exe")
	if err != nil || !filepath.IsAbs(path) {
		return "", false, errors.New("configure Git Bash or PowerShell for the Claude Code status-line shell")
	}
	return path, true, nil
}

func (m claudeUsageIntegration) args(action, manifest string) []string {
	return []string{"worker", "config", "usage", "integrate", claudeUsageMarker, action, manifest}
}

func (m claudeUsageIntegration) statusCommand(manifest string) string {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	prefix := ""
	binary := filepath.ToSlash(m.Binary)
	if m.PowerShell {
		quote = func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
		prefix = "& "
	}
	words := append([]string{binary}, m.args("status-line", manifest)...)
	for i, word := range words {
		words[i] = quote(filepath.ToSlash(word))
	}
	return prefix + strings.Join(words, " ")
}

func installClaudeUsage(ctx context.Context, dir string, desired claudeUsageIntegration,
	discover func(context.Context, string) (ops.UsageAccount, bool)) (bool, error) {
	st, err := store.OpenContext(ctx, desired.StateDir)
	if err != nil {
		return false, err
	}
	defer func() { _ = st.Close() }()
	enrollment, err := st.LoadEnrollment(ctx)
	if err != nil {
		return false, errors.New("enroll the Worker before installing Claude Code usage")
	}
	grant, err := st.UsageGrantID(ctx, enrollment.WorkerID)
	if err != nil || grant == "" {
		return false, errors.New("review and accept monitoring with minion worker config usage --accept before installing the Claude Code integration")
	}
	desired.Grant = claudeUsageGrant(grant)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	release, err := lockConfigFileContext(ctx, filepath.Join(dir, ".minion-usage.lock"))
	if err != nil {
		return false, err
	}
	defer release()
	path := filepath.Join(dir, "settings.json")
	manifest := filepath.Join(dir, claudeUsageManifest)
	settingsRaw, err := readClaudeUsageFile(path, false)
	if err != nil {
		return false, fmt.Errorf("read Claude Code settings: %w", err)
	}
	settings := make(map[string]json.RawMessage)
	if settingsRaw != nil {
		settings, err = claudeUsageObject(settingsRaw)
		if err != nil {
			return false, errors.New("expected a JSON object with unique keys for Claude Code settings")
		}
	}
	if bytes.Equal(bytes.TrimSpace(settings["disableAllHooks"]), []byte("true")) {
		return false, errors.New("this Claude Code profile has disabled hooks; the file was left intact")
	}

	prior, err := readClaudeUsageManifest(manifest)
	if err != nil {
		return false, err
	}
	if desired.Account == "" && desired.Profile == "" && discover != nil {
		if a, ok := discover(ctx, dir); ok && a.Provider == "Anthropic" && a.AuthMode == "subscription" &&
			validateClaudeUsageAttribution(a.ID, "") == nil {
			desired.Account = a.ID
			if len(a.Plan) <= 64 && strings.IndexFunc(a.Plan, unicode.IsControl) < 0 {
				desired.Plan = a.Plan
			}
		}
	}
	if prior != nil {
		desired.PreviousLine = prior.PreviousLine
		// Account plan changes are informational; do not rewrite the profile
		// or treat them as explicit authorization for a different account.
		desired.Plan = prior.Plan
		if desired != *prior {
			return false, errors.New("this Claude Code profile is bound to a different Minion setup; remove its existing integration before rebinding")
		}
	}
	status, oldStatus, err := claudeUsageStatus(settings["statusLine"])
	if err != nil {
		return false, err
	}
	newStatus := desired.statusCommand(manifest)
	if prior == nil {
		if strings.Contains(oldStatus, claudeUsageMarker) {
			return false, errors.New("existing Claude Code status line uses a Minion exporter whose settings cannot be verified")
		}
		desired.PreviousLine = oldStatus
	} else if oldStatus != newStatus && oldStatus != prior.PreviousLine {
		return false, errors.New("the Claude Code status line changed since integration; the file was left intact")
	}

	changed, err := mergeClaudeUsageHooks(settings, desired, manifest)
	if err != nil {
		return false, err
	}
	if newStatus != oldStatus {
		status["type"] = json.RawMessage(`"command"`)
		status["command"], err = json.Marshal(newStatus)
		if err != nil {
			return false, err
		}
		settings["statusLine"], err = json.Marshal(status)
		if err != nil {
			return false, err
		}
		changed = true
	}
	if !changed {
		return false, nil
	}
	next, err := json.MarshalIndent(settings, "", "  ")
	if err != nil || len(next)+1 > maxClaudeSettings {
		return false, errors.New("the combined Claude Code settings exceed the supported file size")
	}

	// Check for a non-Minion writer before replacing the profile settings.
	current, err := readClaudeUsageFile(path, false)
	if err != nil || !bytes.Equal(current, settingsRaw) {
		return false, errors.New("run the integration again because the Claude Code settings changed during setup")
	}
	if prior == nil {
		raw, err := json.Marshal(desired)
		if err != nil || len(raw)+1 > maxClaudeSettings {
			return false, errors.New("the Claude Code usage manifest would exceed the supported file size")
		}
		if err := identity.WritePrivate(manifest, append(raw, '\n')); err != nil {
			return false, fmt.Errorf("save the Claude Code usage manifest: %w", err)
		}
	}
	if err := identity.WritePrivate(path, append(next, '\n')); err != nil {
		// A manifest written first is safe to retry with the same settings.
		return false, fmt.Errorf("save Claude Code settings: %w", err)
	}
	return true, nil
}

func claudeUsageStatus(raw json.RawMessage) (map[string]json.RawMessage, string, error) {
	if len(raw) == 0 {
		return make(map[string]json.RawMessage), "", nil
	}
	obj, err := claudeUsageObject(raw)
	var kind, command string
	if err != nil || json.Unmarshal(obj["type"], &kind) != nil || kind != "command" ||
		json.Unmarshal(obj["command"], &command) != nil || strings.TrimSpace(command) == "" {
		return nil, "", errors.New("the Claude Code statusLine format is unsupported")
	}
	return obj, command, nil
}

func mergeClaudeUsageHooks(settings map[string]json.RawMessage, m claudeUsageIntegration, manifest string) (bool, error) {
	hooks := make(map[string]json.RawMessage)
	if raw, exists := settings["hooks"]; exists {
		var err error
		hooks, err = claudeUsageObject(raw)
		if err != nil {
			return false, errors.New("expected a JSON object for the Claude Code hooks setting")
		}
	}
	changed := false
	for _, event := range claudeUsageEvents {
		var groups []json.RawMessage
		if raw, exists := hooks[event]; exists {
			if json.Unmarshal(raw, &groups) != nil || groups == nil {
				return false, fmt.Errorf("expected a list for Claude Code %s hooks", event)
			}
		}
		hook := claudeUsageHook{
			Type: "command", Command: m.Binary, Args: m.args(event, manifest), Async: event != "SessionEnd",
		}
		found, err := claudeUsageFindHook(groups, hook)
		if err != nil {
			return false, err
		}
		if found {
			continue
		}
		group, err := json.Marshal(map[string][]claudeUsageHook{"hooks": {hook}})
		if err != nil {
			return false, err
		}
		hooks[event], err = json.Marshal(append(groups, group))
		if err != nil {
			return false, err
		}
		changed = true
	}
	if changed {
		var err error
		settings["hooks"], err = json.Marshal(hooks)
		if err != nil {
			return false, err
		}
	}
	return changed, nil
}

func claudeUsageFindHook(groups []json.RawMessage, want claudeUsageHook) (bool, error) {
	found := false
	for _, raw := range groups {
		var group struct {
			Matcher string            `json:"matcher"`
			Hooks   []claudeUsageHook `json:"hooks"`
		}
		if json.Unmarshal(raw, &group) != nil || group.Hooks == nil {
			return false, errors.New("an existing Claude Code hook group has an unsupported format")
		}
		for _, rule := range group.Hooks {
			if !slices.Contains(rule.Args, claudeUsageMarker) && !strings.Contains(rule.Command, claudeUsageMarker) {
				continue
			}
			if found || group.Matcher != "" || rule.Command != want.Command || rule.Type != want.Type ||
				!slices.Equal(rule.Args, want.Args) || rule.Async != want.Async {
				return false, errors.New("a conflicting Minion usage hook already exists in this Claude Code profile")
			}
			found = true
		}
	}
	return found, nil
}

func claudeUsageObject(raw []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.New("expected a JSON object")
	}
	obj := make(map[string]json.RawMessage)
	for dec.More() {
		key, err := dec.Token()
		name, ok := key.(string)
		if err != nil || !ok || obj[name] != nil {
			return nil, errors.New("invalid or duplicate JSON key")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		obj[name] = v
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim('}') {
		return nil, errors.New("invalid JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after JSON object")
	}
	return obj, nil
}

func readClaudeUsageFile(path string, private bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxClaudeSettings {
		return nil, errors.New("expected a regular, bounded file in the selected Claude Code profile")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	current, err := f.Stat()
	if err != nil || !os.SameFile(info, current) {
		return nil, errors.New("the Claude Code file changed before it could be read")
	}
	if private {
		if err := identity.CheckPrivateFile(f); err != nil {
			return nil, errors.New("the Claude Code usage manifest must be private")
		}
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxClaudeSettings+1))
	if err != nil || len(raw) > maxClaudeSettings {
		return nil, errors.New("the Claude Code file could not be read within its size limit")
	}
	return raw, nil
}

func readClaudeUsageManifest(path string) (*claudeUsageIntegration, error) {
	raw, err := readClaudeUsageFile(path, true)
	if err != nil || raw == nil {
		return nil, err
	}
	var m claudeUsageIntegration
	if json.Unmarshal(raw, &m) != nil || m.Version != 1 || !filepath.IsAbs(m.Binary) || !filepath.IsAbs(m.StateDir) ||
		!filepath.IsAbs(m.Shell) || len(m.Grant) != 64 || validateClaudeUsageAttribution(m.Account, m.Profile) != nil {
		return nil, errors.New("the Claude Code usage manifest is invalid")
	}
	return &m, nil
}
