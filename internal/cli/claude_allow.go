package cli

// "minion claude allow | status | revoke": standing, owner-approved
// permissions for Claude Code to use the Minion MCP tools on chosen targets
// without a prompt per call.
//
// Two files change together:
//
//   - the user settings of Claude Code (~/.claude/settings.json, or the
//     profile of CLAUDE_CONFIG_DIR / --claude-config-dir): one
//     permissions.allow entry mcp__<server>__<op> per op. Claude Code applies
//     allow rules before the auto mode classifier, so those tool calls stop
//     prompting. A rule for an MCP tool cannot name its arguments.
//   - the Minion allowlist (claudeallow.DefaultPath): the rule id, the ops and
//     the target Workers and sessions. "minion mcp" reads it on every write
//     call and refuses a write to any other target before it is sent, so the
//     target limit is Minion's own.
//
// Only user scope is written, never a project file. allow refuses to run
// inside a Claude Code session (CLAUDECODE is set): the agent must not grant
// itself standing permissions; the owner runs it in a terminal of their own.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/claudeallow"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/mcp"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// claudeAllowDefaultOps are the ops a rule allows when --ops is not given:
// the session ops of a master session, reads and writes. Never an admin op.
var claudeAllowDefaultOps = []string{
	ops.OpSendText, ops.OpSendKeys, ops.OpAnswer, ops.OpSessionSnapshot, ops.OpSessionCapture,
	ops.OpSessionsList, ops.OpWorkersList, ops.OpPipeSend, ops.OpPipeRead,
}

// claudePluginName is the plugin that registers "minion mcp" as server
// "minion"; Claude Code names its tools mcp__plugin_minion-control_minion__*.
const claudePluginName = "minion-control"

// claudeDefaultServer is the default MCP server name of "minion mcp".
const claudeDefaultServer = "minion"

// claudeDeps are the inputs of "minion claude" that tests replace.
type claudeDeps struct {
	stdin     io.Reader
	lookupEnv func(string) (string, bool)
	// interactive reports whether stdin and stdout are terminals.
	interactive func(in io.Reader, out io.Writer) bool
	now         func() time.Time
	// allowlist is the path of the allowlist; "" is claudeallow.DefaultPath.
	allowlist string
}

func defaultClaudeDeps() claudeDeps {
	return claudeDeps{
		stdin:     os.Stdin,
		lookupEnv: os.LookupEnv,
		interactive: func(in io.Reader, out io.Writer) bool {
			return isTerminalStream(in) && isTerminalWriter(out)
		},
		now: time.Now,
	}
}

func init() {
	Register(func(root *cobra.Command) {
		root.AddCommand(newClaudeCmd(defaultClaudeDeps()))
	})
}

func newClaudeCmd(deps claudeDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "claude",
		Short: "Let Claude Code use Minion on chosen targets without a prompt per command",
		Long: `Standing, owner-approved permissions for Claude Code.

"minion claude allow" adds one permissions.allow entry per Minion MCP tool to
the Claude Code user settings (~/.claude/settings.json) and records the rule,
with the Workers and sessions it reaches, in the Minion allowlist. "minion mcp"
then refuses a write to any other target before it is sent, and the Hub audit
names the rule of every write sent under it. "minion claude revoke" removes a
rule from both files at once; "minion claude status" lists them.

Minion's own checks (sharing, grants, approvals) are unchanged: a rule only
saves the prompt of Claude Code for calls Minion allows anyway.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newClaudeAllowCmd(deps), newClaudeStatusCmd(deps), newClaudeRevokeCmd(deps))
	return cmd
}

func newClaudeAllowCmd(deps claudeDeps) *cobra.Command {
	var (
		workers, sessions, opList []string
		server, profile           string
		yes                       bool
	)
	cmd := &cobra.Command{
		Use:   "allow --worker ID [--session NAME]... [--ops OP,...] [--yes]",
		Short: "Add a standing allow rule for the Minion MCP tools on chosen targets",
		Long: `Add a standing allow rule. It shows exactly what it adds and asks for
confirmation on the terminal (--yes skips the question, for scripts; without a
terminal and without --yes nothing is written).

--worker names a Worker by its id (UUIDv7, as workers_list prints it); repeat it
for more Workers. --session limits the rule to sessions of those Workers;
without it the rule reaches every session of them. --ops lists the MCP tools
(default: ` + strings.Join(claudeAllowDefaultOps, ", ") + `).

The rule is written to the Claude Code user settings only (never a project
file) and to the Minion allowlist. It refuses to run inside a Claude Code
session: run it in a terminal of your own.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClaudeAllow(cmd.Context(), cmd, deps, claudeAllowRequest{
				workers: workers, sessions: sessions, ops: opList, server: server, profile: profile, yes: yes,
				opsSet: cmd.Flags().Changed("ops"),
			})
		},
	}
	cmd.Flags().StringArrayVar(&workers, "worker", nil, "Worker id (UUIDv7) the rule reaches (repeatable, required)")
	cmd.Flags().StringArrayVar(&sessions, "session", nil, "Session name the rule reaches on those Workers (repeatable; default: every session)")
	cmd.Flags().StringSliceVar(&opList, "ops", nil, "MCP tools the rule allows, comma separated (default: the session ops)")
	cmd.Flags().StringVar(&server, "mcp-server-name", claudeDefaultServer, "Name of the minion mcp server in Claude Code")
	cmd.Flags().StringVar(&profile, "claude-config-dir", "", "Claude Code profile directory (defaults to CLAUDE_CONFIG_DIR or ~/.claude)")
	cmd.Flags().BoolVar(&yes, "yes", false, "Do not ask for confirmation")
	return cmd
}

func newClaudeStatusCmd(deps claudeDeps) *cobra.Command {
	var (
		profile string
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "status [--json]",
		Short: "List the standing allow rules and their Claude Code settings entries",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := claudeAllowState(deps, profile)
			if err != nil {
				return err
			}
			if asJSON {
				b, err := json.MarshalIndent(st, "", "  ")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return err
			}
			return printClaudeStatus(cmd.OutOrStdout(), st)
		},
	}
	cmd.Flags().StringVar(&profile, "claude-config-dir", "", "Claude Code profile directory (defaults to CLAUDE_CONFIG_DIR or ~/.claude)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print JSON")
	return cmd
}

func newClaudeRevokeCmd(deps claudeDeps) *cobra.Command {
	var (
		profile string
		all     bool
	)
	cmd := &cobra.Command{
		Use:   "revoke RULE_ID | --all",
		Short: "Remove a standing allow rule from the allowlist and the Claude Code settings",
		Long: `Remove a standing allow rule (or every rule with --all). Its settings
entries are removed unless a remaining rule needs them; an entry that was in the
settings before the rule was added stays. "minion mcp" reads the allowlist on
every write call, so the revoke applies to the next call. A backup of the
replaced settings is kept in settings.json.minion-backup.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if all == (len(args) == 1) {
				return errors.New("name one rule id, or give --all")
			}
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			return runClaudeRevoke(cmd.Context(), cmd.OutOrStdout(), deps, profile, id)
		},
	}
	cmd.Flags().StringVar(&profile, "claude-config-dir", "", "Claude Code profile directory (defaults to CLAUDE_CONFIG_DIR or ~/.claude)")
	cmd.Flags().BoolVar(&all, "all", false, "Remove every standing allow rule")
	return cmd
}

type claudeAllowRequest struct {
	workers, sessions, ops []string
	opsSet                 bool
	server, profile        string
	yes                    bool
}

// claudeMCPTools returns the tools "minion mcp" can expose.
func claudeMCPTools() map[string]bool {
	out := map[string]bool{}
	for _, op := range mcp.DefaultOps() {
		out[op.MCPTool()] = true
	}
	return out
}

// claudeAllowOps validates the requested ops and returns them with the
// warnings to show.
func claudeAllowOps(req claudeAllowRequest) ([]string, []string, error) {
	if !req.opsSet {
		return slices.Clone(claudeAllowDefaultOps), nil, nil
	}
	tools := claudeMCPTools()
	var out, warnings []string
	for _, name := range req.ops {
		name = strings.TrimSpace(name)
		if name == "" || slices.Contains(out, name) {
			continue
		}
		if !tools[name] {
			return nil, nil, fmt.Errorf("%q is not a tool of minion mcp; it stays on the prompting path", name)
		}
		if !slices.Contains(claudeAllowDefaultOps, name) {
			warnings = append(warnings, fmt.Sprintf("%s is not one of the default session ops: Claude Code will run it on the chosen targets without asking", name))
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, nil, errors.New("--ops names no tool")
	}
	return out, warnings, nil
}

func claudeAllowTargets(workers, sessions []string) ([]claudeallow.Target, error) {
	if len(workers) == 0 {
		return nil, errors.New("name at least one Worker with --worker (its id, as workers_list prints it)")
	}
	var names []string
	for _, s := range sessions {
		if (ops.Target{SessionName: s}).Validate() != nil {
			return nil, fmt.Errorf("--session %q is not a valid session name", s)
		}
		if !slices.Contains(names, s) {
			names = append(names, s)
		}
	}
	var out []claudeallow.Target
	seen := map[string]bool{}
	for _, w := range workers {
		w = strings.ToLower(strings.TrimSpace(w))
		if !ops.ValidUUIDv7(w) {
			return nil, fmt.Errorf("--worker %q is not a Worker id (a UUIDv7; workers_list prints them)", w)
		}
		if seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, claudeallow.Target{WorkerID: w, Sessions: slices.Clone(names)})
	}
	return out, nil
}

// claudePluginInstalled reports whether the minion-control plugin is
// installed in the profile dir (plugins/installed_plugins.json names
// "minion-control@<marketplace>").
func claudePluginInstalled(dir string) bool {
	raw, err := readClaudeUsageFile(filepath.Join(dir, "plugins", "installed_plugins.json"), false)
	if err != nil || raw == nil {
		return false
	}
	var f struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return false
	}
	for key := range f.Plugins {
		if name, _, _ := strings.Cut(key, "@"); name == claudePluginName {
			return true
		}
	}
	return false
}

// claudeServers returns the server names whose tools a rule allows: the
// given name, and the plugin-scoped name when the plugin is installed and
// the default name is used.
func claudeServers(dir, server string) []string {
	out := []string{server}
	if server == claudeDefaultServer && claudePluginInstalled(dir) {
		out = append(out, claudeallow.PluginServer(claudePluginName, claudeDefaultServer))
	}
	return out
}

func claudeEntries(servers, opNames []string) []string {
	var out []string
	for _, s := range servers {
		for _, op := range opNames {
			out = append(out, claudeallow.ToolEntry(s, op))
		}
	}
	return out
}

func (d claudeDeps) allowlistPath() (string, error) {
	if d.allowlist != "" {
		return d.allowlist, nil
	}
	return claudeallow.DefaultPath()
}

// claudeSettings is the user settings file of a profile, read for an edit.
type claudeSettings struct {
	path     string
	raw      []byte // nil: no file
	top      map[string]json.RawMessage
	perms    map[string]json.RawMessage
	allow    []string
	allowRaw []json.RawMessage
}

func readClaudeSettings(dir string) (*claudeSettings, error) {
	s := &claudeSettings{path: filepath.Join(dir, "settings.json"), top: map[string]json.RawMessage{}, perms: map[string]json.RawMessage{}}
	raw, err := readClaudeUsageFile(s.path, false)
	if err != nil {
		return nil, fmt.Errorf("read Claude Code settings: %w", err)
	}
	s.raw = raw
	if raw == nil {
		return s, nil
	}
	if s.top, err = claudeUsageObject(raw); err != nil {
		return nil, fmt.Errorf("the Claude Code settings %s are not a JSON object with unique keys; nothing was changed", s.path)
	}
	if p := s.top["permissions"]; len(p) > 0 && string(bytes.TrimSpace(p)) != "null" {
		if s.perms, err = claudeUsageObject(p); err != nil {
			return nil, fmt.Errorf("permissions in %s is not a JSON object; nothing was changed", s.path)
		}
	}
	if a := s.perms["allow"]; len(a) > 0 && string(bytes.TrimSpace(a)) != "null" {
		if err := json.Unmarshal(a, &s.allowRaw); err != nil {
			return nil, fmt.Errorf("permissions.allow in %s is not a JSON array; nothing was changed", s.path)
		}
		for _, e := range s.allowRaw {
			var str string
			_ = json.Unmarshal(e, &str) // a non-string entry is kept as it is
			s.allow = append(s.allow, str)
		}
	}
	return s, nil
}

func (s *claudeSettings) has(entry string) bool { return slices.Contains(s.allow, entry) }

// edit returns the settings with permissions.allow made of the entries
// that are not in remove, then add, in order.
func (s *claudeSettings) edit(add []string, remove map[string]bool) ([]byte, error) {
	var out []json.RawMessage
	for i, e := range s.allowRaw {
		if remove[s.allow[i]] && s.allow[i] != "" {
			continue
		}
		out = append(out, e)
	}
	for _, e := range add {
		b, _ := json.Marshal(e)
		out = append(out, b)
	}
	if out == nil {
		out = []json.RawMessage{}
	}
	allow, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	s.perms["allow"] = allow
	perms, err := json.Marshal(s.perms)
	if err != nil {
		return nil, err
	}
	s.top["permissions"] = perms
	next, err := json.MarshalIndent(s.top, "", "  ")
	if err != nil || len(next)+1 > maxClaudeSettings {
		return nil, errors.New("the Claude Code settings would exceed the supported file size")
	}
	return append(next, '\n'), nil
}

// save replaces the settings with data, keeping a backup, when the file is
// still the one that was read.
func (s *claudeSettings) save(data []byte) error {
	unchanged := func() error {
		current, err := readClaudeUsageFile(s.path, false)
		if err != nil || !bytes.Equal(current, s.raw) {
			return errClaudeSettingsChanged
		}
		return nil
	}
	if err := unchanged(); err != nil {
		return err
	}
	return saveClaudeSettings(s.path, data, unchanged)
}

func claudeAllowProfile(deps claudeDeps, explicit string) (string, error) {
	return claudeUsageProfileDir(explicit, deps.lookupEnv)
}

func runClaudeAllow(ctx context.Context, cmd *cobra.Command, deps claudeDeps, req claudeAllowRequest) error {
	out := cmd.OutOrStdout()
	if v, ok := deps.lookupEnv("CLAUDECODE"); ok && v != "" {
		return errors.New("minion claude allow does not run inside a Claude Code session: an agent must not grant itself standing permissions. Run it in a terminal of your own")
	}
	opNames, warnings, err := claudeAllowOps(req)
	if err != nil {
		return err
	}
	targets, err := claudeAllowTargets(req.workers, req.sessions)
	if err != nil {
		return err
	}
	if req.server == "" || strings.ContainsAny(req.server, " \t\n") {
		return errors.New("--mcp-server-name must be the name of the minion mcp server in Claude Code, without spaces")
	}
	dir, err := claudeAllowProfile(deps, req.profile)
	if err != nil {
		return err
	}
	listPath, err := deps.allowlistPath()
	if err != nil {
		return err
	}
	release, err := lockClaudeAllow(ctx, dir, listPath)
	if err != nil {
		return err
	}
	defer release()

	list, err := claudeallow.Load(listPath)
	if err != nil {
		return fmt.Errorf("%w; fix or remove %s first", err, listPath)
	}
	if list == nil {
		list = &claudeallow.File{Version: claudeallow.Version}
	}
	settings, err := readClaudeSettings(dir)
	if err != nil {
		return err
	}
	entries := claudeEntries(claudeServers(dir, req.server), opNames)
	var added []string
	for _, e := range entries {
		if !settings.has(e) {
			added = append(added, e)
		}
	}
	rule := claudeallow.Rule{
		ID: ops.NewUUIDv7(), Created: deps.now().UTC().Truncate(time.Second),
		Ops: opNames, Targets: targets, Settings: settings.path, Entries: entries, Added: added,
	}

	_, _ = fmt.Fprintf(out, "Standing allow rule %s\n", rule.ID)
	_, _ = fmt.Fprintf(out, "  targets: %s\n", rule.Describe())
	_, _ = fmt.Fprintf(out, "  ops:     %s\n", strings.Join(opNames, ", "))
	_, _ = fmt.Fprintf(out, "Claude Code settings %s, permissions.allow:\n", settings.path)
	for _, e := range entries {
		mark := "+"
		if !slices.Contains(added, e) {
			mark = "=" // already there
		}
		b, _ := json.Marshal(e)
		_, _ = fmt.Fprintf(out, "  %s %s\n", mark, b)
	}
	_, _ = fmt.Fprintf(out, "Minion allowlist %s: + rule %s\n", listPath, rule.ID)
	for _, w := range warnings {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
	}
	_, _ = fmt.Fprintln(out, "Claude Code then runs these Minion tools without asking; minion mcp refuses these tools on any other target (sessions_list and workers_list name none).")
	if !req.yes {
		if deps.interactive == nil || !deps.interactive(deps.stdin, out) {
			return errors.New("nothing was written: confirm on a terminal, or give --yes")
		}
		_, _ = fmt.Fprint(out, "Add this rule? [y/N] ")
		line, _ := bufio.NewReader(deps.stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			_, _ = fmt.Fprintln(out, "Nothing was written.")
			return nil
		}
	}
	data, err := settings.edit(added, nil)
	if err != nil {
		return err
	}
	// The allowlist first: Claude Code settings without the allowlist would
	// let minion mcp send to any target.
	list.Rules = append(list.Rules, rule)
	if err := writeClaudeAllowlist(listPath, list); err != nil {
		return err
	}
	if len(added) > 0 {
		if err := settings.save(data); err != nil {
			return fmt.Errorf("the rule is in the Minion allowlist, but the Claude Code settings were not changed: %w", err)
		}
	}
	_, _ = fmt.Fprintf(out, "Added rule %s. Revoke it with: minion claude revoke %s\n", rule.ID, rule.ID)
	_, _ = fmt.Fprintln(out, "Start a new Claude Code session (or run /mcp) so that minion mcp offers its write tools.")
	return nil
}

// lockClaudeAllow takes the locks of a change of the allowlist at listPath and
// the settings of the Claude Code profile dir: first the lock next to the
// allowlist, which every profile shares (the allowlist is one file for all of
// them), then the lock of the profile, which an older minion takes alone.
// The returned function releases both.
func lockClaudeAllow(ctx context.Context, dir, listPath string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(listPath), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	releaseList, err := lockConfigFileContext(ctx, claudeallow.LockPath(listPath))
	if err != nil {
		return nil, err
	}
	releaseProfile, err := lockConfigFileContext(ctx, filepath.Join(dir, ".minion-claude-allow.lock"))
	if err != nil {
		releaseList()
		return nil, err
	}
	return func() {
		releaseProfile()
		releaseList()
	}, nil
}

func writeClaudeAllowlist(path string, list *claudeallow.File) error {
	if len(list.Rules) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove the Minion allowlist: %w", err)
		}
		return nil
	}
	data, err := list.Encode()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := identity.WritePrivate(path, data); err != nil {
		return fmt.Errorf("save the Minion allowlist: %w", err)
	}
	return nil
}

func runClaudeRevoke(ctx context.Context, out io.Writer, deps claudeDeps, profile, id string) error {
	dir, err := claudeAllowProfile(deps, profile)
	if err != nil {
		return err
	}
	listPath, err := deps.allowlistPath()
	if err != nil {
		return err
	}
	release, err := lockClaudeAllow(ctx, dir, listPath)
	if err != nil {
		return err
	}
	defer release()
	list, err := claudeallow.Load(listPath)
	if err != nil {
		return fmt.Errorf("%w; fix or remove %s", err, listPath)
	}
	if list == nil || len(list.Rules) == 0 {
		if id != "" {
			return fmt.Errorf("no standing allow rule %s", id)
		}
		_, _ = fmt.Fprintln(out, "No standing allow rule.")
		return nil
	}
	var removed []claudeallow.Rule
	if id == "" {
		removed, list.Rules = list.Rules, nil
	} else {
		r, ok := list.Rule(id)
		if !ok {
			return fmt.Errorf("no standing allow rule %s (minion claude status lists them)", id)
		}
		list.Remove(id)
		removed = []claudeallow.Rule{r}
	}
	need := map[string]bool{}
	for _, e := range list.Entries() {
		need[e] = true
	}
	drop := map[string]bool{}
	for _, r := range removed {
		for _, e := range r.Added {
			if !need[e] {
				drop[e] = true
				continue
			}
			// A remaining rule needs the entry: it now owns it, so that its
			// own revoke removes the entry Minion added.
			for i := range list.Rules {
				if slices.Contains(list.Rules[i].Entries, e) && !slices.Contains(list.Rules[i].Added, e) {
					list.Rules[i].Added = append(list.Rules[i].Added, e)
					break
				}
			}
		}
	}
	// The settings first: once they no longer allow the tools, Claude Code
	// asks again, whatever the allowlist still says.
	settings, err := readClaudeSettings(dir)
	if err != nil {
		return err
	}
	changed := false
	for e := range drop {
		if settings.has(e) {
			changed = true
		}
	}
	if changed {
		data, err := settings.edit(nil, drop)
		if err != nil {
			return err
		}
		if err := settings.save(data); err != nil {
			return fmt.Errorf("nothing was revoked: %w", err)
		}
	}
	if err := writeClaudeAllowlist(listPath, list); err != nil {
		return err
	}
	for _, r := range removed {
		_, _ = fmt.Fprintf(out, "Revoked rule %s (%s).\n", r.ID, r.Describe())
	}
	if len(drop) > 0 {
		names := make([]string, 0, len(drop))
		for e := range drop {
			names = append(names, e)
		}
		sort.Strings(names)
		_, _ = fmt.Fprintf(out, "Removed from %s: %s\n", settings.path, strings.Join(names, ", "))
	}
	return nil
}

// claudeStatus is the output of "minion claude status --json".
type claudeStatus struct {
	Allowlist string               `json:"allowlist"`
	Settings  string               `json:"settings"`
	Rules     []claudeStatusRule   `json:"rules"`
	Unmanaged []string             `json:"unmanaged_entries"`
	Error     string               `json:"error,omitempty"`
	Targets   []claudeallow.Target `json:"targets"`
}

type claudeStatusRule struct {
	ID      string               `json:"id"`
	Created time.Time            `json:"created"`
	Ops     []string             `json:"ops"`
	Targets []claudeallow.Target `json:"targets"`
	Summary string               `json:"summary"`
	Entries []claudeStatusEntry  `json:"entries"`
}

type claudeStatusEntry struct {
	Entry   string `json:"entry"`
	Present bool   `json:"present"`
}

// claudeAllowState reads both files. An entry for a Minion server in the
// settings that no rule needs is listed as unmanaged.
func claudeAllowState(deps claudeDeps, profile string) (*claudeStatus, error) {
	dir, err := claudeAllowProfile(deps, profile)
	if err != nil {
		return nil, err
	}
	listPath, err := deps.allowlistPath()
	if err != nil {
		return nil, err
	}
	settings, err := readClaudeSettings(dir)
	if err != nil {
		return nil, err
	}
	st := &claudeStatus{Allowlist: listPath, Settings: settings.path, Rules: []claudeStatusRule{}, Unmanaged: []string{}, Targets: []claudeallow.Target{}}
	list, err := claudeallow.Load(listPath)
	if err != nil {
		// minion mcp refuses every write with such a file.
		st.Error = err.Error()
	}
	need := map[string]bool{}
	if list != nil {
		for _, r := range list.Rules {
			sr := claudeStatusRule{ID: r.ID, Created: r.Created, Ops: r.Ops, Targets: r.Targets, Summary: r.Describe(), Entries: []claudeStatusEntry{}}
			for _, e := range r.Entries {
				need[e] = true
				sr.Entries = append(sr.Entries, claudeStatusEntry{Entry: e, Present: settings.has(e)})
			}
			st.Rules = append(st.Rules, sr)
			st.Targets = append(st.Targets, r.Targets...)
		}
	}
	prefixes := []string{"mcp__" + claudeDefaultServer + "__", "mcp__" + claudeallow.PluginServer(claudePluginName, claudeDefaultServer) + "__"}
	for _, r := range st.Rules {
		for _, e := range r.Entries {
			if i := strings.LastIndex(e.Entry, "__"); i > 0 {
				if p := e.Entry[:i+2]; !slices.Contains(prefixes, p) {
					prefixes = append(prefixes, p)
				}
			}
		}
	}
	for _, e := range settings.allow {
		if need[e] {
			continue
		}
		for _, p := range prefixes {
			if strings.HasPrefix(e, p) || e == strings.TrimSuffix(p, "__") {
				st.Unmanaged = append(st.Unmanaged, e)
				break
			}
		}
	}
	return st, nil
}

func printClaudeStatus(out io.Writer, st *claudeStatus) error {
	_, _ = fmt.Fprintf(out, "Minion allowlist: %s\n", st.Allowlist)
	_, _ = fmt.Fprintf(out, "Claude Code settings: %s\n", st.Settings)
	if st.Error != "" {
		_, _ = fmt.Fprintf(out, "The allowlist cannot be read (%s): minion mcp refuses every write, and every read with a target, until it is fixed or removed.\n", st.Error)
	}
	if len(st.Rules) == 0 {
		_, _ = fmt.Fprintln(out, "No standing allow rule: minion mcp behaves as without minion claude allow.")
	}
	for _, r := range st.Rules {
		_, _ = fmt.Fprintf(out, "\nrule %s (created %s)\n", r.ID, r.Created.Format(time.RFC3339))
		_, _ = fmt.Fprintf(out, "  targets: %s\n", r.Summary)
		_, _ = fmt.Fprintf(out, "  ops:     %s\n", strings.Join(r.Ops, ", "))
		for _, e := range r.Entries {
			state := "present"
			if !e.Present {
				state = "MISSING from the settings (Claude Code asks for this tool)"
			}
			_, _ = fmt.Fprintf(out, "  %s: %s\n", e.Entry, state)
		}
	}
	if len(st.Unmanaged) > 0 {
		_, _ = fmt.Fprintln(out, "\nMinion tool entries in the settings that no rule needs (added by hand; with no rule left, minion mcp does not limit their targets):")
		for _, e := range st.Unmanaged {
			_, _ = fmt.Fprintf(out, "  %s\n", e)
		}
	}
	return nil
}
