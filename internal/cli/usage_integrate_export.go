package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/spf13/cobra"
)

const maxClaudeUsagePayload = 256 << 10

var claudeUsagePane = regexp.MustCompile(`^%[0-9]{1,10}$`)

func claudeUsageGrant(grant string) string {
	return ops.UsageID("claude-profile-grant", grant)
}

func newClaudeUsageExportCmd(lookup func(string) (string, bool)) *cobra.Command {
	return &cobra.Command{
		Use: claudeUsageMarker + " <event> <manifest>", Hidden: true,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, manifest := args[0], args[1]
			if !filepath.IsAbs(manifest) || filepath.Base(manifest) != claudeUsageManifest ||
				(mode != "status-line" && !slices.Contains(claudeUsageEvents, mode)) {
				return nil
			}
			m, err := readClaudeUsageManifest(manifest)
			if err != nil || m == nil {
				return nil // a missing exporter never blocks a regular Claude hook
			}
			profile := filepath.Dir(manifest)
			return dispatchClaudeUsage(cmd.Context(), *m, mode, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), func(ctx context.Context, payload []byte, at time.Time) {
				if lookup != nil {
					dir, _ := lookup("CLAUDE_CONFIG_DIR")
					if dir != "" && !sameDir(dir, profile) {
						return
					}
				}
				session, pane, created, pid, birth := harnessPaneProcess(ctx, lookup, m.StateDir, "claude")
				if session == "" {
					// Older hosts can still provide the original usage attribution.
					session = claudeUsageSession(ctx, lookup)
				}
				recordBoundClaudeUsage(ctx, *m, mode, session, pane, created, pid, birth, claudeUsageAccess(lookup, profile), payload, at, lookup, profile)
			})
		},
	}
}

// dispatchClaudeUsage keeps the native payload only in memory, and only passes
// it through to the user's original status line. No Minion output represents
// a hook decision. The original command's stdout, stderr and exit status keep
// their meaning even when recording fails or the input exceeds our limit.
func dispatchClaudeUsage(ctx context.Context, m claudeUsageIntegration, mode string, in io.Reader, out, errOut io.Writer,
	record func(context.Context, []byte, time.Time)) error {
	at := time.Now()
	raw, err := io.ReadAll(io.LimitReader(in, maxClaudeUsagePayload+1))
	arrived := err == nil && len(raw) > 0 && len(raw) <= maxClaudeUsagePayload
	if mode != "status-line" || m.PreviousLine == "" {
		if arrived {
			recCtx, cancel := context.WithTimeout(ctx, 900*time.Millisecond)
			defer cancel()
			record(recCtx, raw, at)
		}
		return nil
	}

	var finished chan struct{}
	if arrived {
		recCtx, cancel := context.WithTimeout(ctx, 900*time.Millisecond)
		defer cancel()
		finished = make(chan struct{})
		go func() {
			defer close(finished)
			record(recCtx, raw, at)
		}()
		defer func() {
			select {
			case <-finished:
			case <-recCtx.Done():
			}
		}()
	}

	shellArgs := []string{"-c", m.PreviousLine}
	if m.PowerShell {
		shellArgs = []string{"-NoProfile", "-NonInteractive", "-Command", m.PreviousLine}
	}
	prior := exec.CommandContext(ctx, m.Shell, shellArgs...)
	prior.Stdin = io.MultiReader(bytes.NewReader(raw), in)
	prior.Stdout, prior.Stderr = out, errOut
	prior.WaitDelay = time.Second
	err = prior.Run()
	var result *exec.ExitError
	if errors.As(err, &result) && result.ExitCode() >= 0 {
		return &exitCodeError{code: result.ExitCode()}
	}
	if err != nil {
		return &exitCodeError{code: ExitError}
	}
	return nil
}

func claudeUsageSession(ctx context.Context, lookup func(string) (string, bool)) string {
	if lookup == nil {
		return ""
	}
	pane, _ := lookup("TMUX_PANE")
	if !claudeUsagePane.MatchString(pane) {
		return ""
	}
	binary, err := exec.LookPath("tmux")
	if err != nil || !filepath.IsAbs(binary) {
		return ""
	}
	cmd := exec.CommandContext(ctx, binary, "display-message", "-p", "-t", pane, "#{session_name}")
	cmd.WaitDelay = 100 * time.Millisecond
	stdout, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return ""
	}
	name, err := io.ReadAll(io.LimitReader(stdout, 258))
	_ = stdout.Close()
	if waitErr := cmd.Wait(); err != nil || waitErr != nil || len(name) > 257 {
		return ""
	}
	return strings.TrimSuffix(string(name), "\n")
}

// claudeUsageAccess is what the exporter itself can verify about the Claude
// Code session that started it: it inherits that process's environment, so an
// API key, a token or a custom endpoint set there (or in the profile's
// settings) is seen first-hand. The profile's subscription is attributed only
// when none of them is set; without an environment to read, the attribution
// stays unknown.
type claudeAccessEvidence int

const (
	claudeAccessUnverified claudeAccessEvidence = iota
	claudeAccessSubscription
	claudeAccessAPI
	claudeAccessCustomEndpoint
)

func claudeUsageAccess(lookup func(string) (string, bool), profile string) claudeAccessEvidence {
	if lookup == nil {
		return claudeAccessUnverified
	}
	key, custom := agent.ClaudeExporterAPIConfig(lookup, profile)
	switch {
	case custom:
		return claudeAccessCustomEndpoint
	case key:
		return claudeAccessAPI
	}
	return claudeAccessSubscription
}

// claudeShowsSubscription reports whether a status line document carries
// subscription rate limits: only a Claude.ai login gets them, so they are the
// evidence of this very call that the session runs on the subscription (a
// Console key saved through /login sets no environment variable or helper).
func claudeShowsSubscription(raw []byte) bool {
	var doc struct {
		Limits map[string]json.RawMessage `json:"rate_limits"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	for name := range doc.Limits {
		if name != "spend_limit" {
			return true
		}
	}
	return false
}

func recordClaudeUsage(ctx context.Context, m claudeUsageIntegration, mode, session string, access claudeAccessEvidence, raw []byte, at time.Time) {
	recordClaudeUsageAt(ctx, m, mode, session, access, raw, at)
}

func recordClaudeUsageAt(ctx context.Context, m claudeUsageIntegration, mode, session string, access claudeAccessEvidence, raw []byte, at time.Time) {
	recordBoundClaudeUsage(ctx, m, mode, session, "", 0, 0, "", access, raw, at, nil, "")
}

func recordBoundClaudeUsage(ctx context.Context, m claudeUsageIntegration, mode, session, pane string, created int64, pid int, birth string, access claudeAccessEvidence, raw []byte, at time.Time, lookup func(string) (string, bool), profile string) {
	if session == "" || ctx.Err() != nil {
		return
	}
	if _, err := os.Stat(filepath.Join(m.StateDir, store.DBFile)); err != nil {
		return // the former Worker was removed or no longer answers
	}
	st, err := store.OpenContext(ctx, m.StateDir)
	if err != nil {
		return
	}
	defer func() { _ = st.Close() }()
	enrollment, err := st.LoadEnrollment(ctx)
	if err != nil {
		return
	}
	grant, err := st.UsageGrantID(ctx, enrollment.WorkerID)
	if err != nil || grant == "" || claudeUsageGrant(grant) != m.Grant {
		return
	}

	if pane != "" && created > 0 && claudeStateEnabled(m.StateDir, lookup) {
		if event, ok := parseClaudeHarnessEvent(mode, raw, grant, at); ok {
			event.Session, event.Pane, event.Created = session, pane, created
			event.ProcessPID, event.ProcessBirth = pid, birth
			if st.RecordHarnessEvent(ctx, grant, event) == nil && mode == "status-line" {
				recordClaudeTitle(ctx, st, grant, profile, event, raw)
			}
		}
	}
	opts := agent.NativeUsageOptions{Format: "claude-statusline", Session: session, Provider: "Anthropic", CustomEndpoint: access == claudeAccessCustomEndpoint, APIKey: access == claudeAccessAPI}
	if mode != "status-line" {
		var event struct {
			Name     string `json:"hook_event_name"`
			PromptID string `json:"prompt_id"`
		}
		if json.Unmarshal(raw, &event) != nil || event.Name != mode {
			return
		}
		opts.Format = "claude-hook"
		if mode == "UserPromptSubmit" || mode == "Stop" {
			opts.EventID = event.PromptID
		}
	}
	// A subscription the installer discovered earlier is attached only when
	// this call shows it (rate limits). A saved Console key leaves no other
	// trace, so without the evidence the session stays unattributed instead of
	// landing on an old subscription. An account the owner bound explicitly
	// stays theirs.
	if access == claudeAccessSubscription && m.Auto && !claudeShowsSubscription(raw) {
		access = claudeAccessUnverified
	}
	if (m.Account != "" || m.Profile != "") && access == claudeAccessSubscription {
		id, label := m.Account, "Anthropic subscription"
		if m.Label != "" {
			label = m.Label // the verified login, not a generic name
		}
		if m.Profile != "" {
			id = ops.UsageID("operator-subscription-profile", hex.EncodeToString(enrollment.HubPub), opts.Provider, m.Profile)
			label = m.Profile
		}
		opts.Account = &ops.UsageAccount{ID: id, Label: label, Plan: m.Plan, Provider: opts.Provider, AuthMode: "subscription", Source: "local-attribution"}
	}
	now := time.Now()
	r, err := agent.ParseNativeUsage(raw, opts, at)
	if err == nil {
		_ = st.SaveNativeUsage(ctx, enrollment, r, now)
	}
}
