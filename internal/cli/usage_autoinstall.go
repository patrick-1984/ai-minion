package cli

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// claudeAutoInstallEvery is how often a running Worker looks again, which also
// covers a profile that appears after the start.
const claudeAutoInstallEvery = 10 * time.Minute

// claudeAutoInstallPending is how often it looks while the Worker is not yet
// enrolled or usage monitoring is not yet accepted, so a newly joined Worker
// reports usage within a minute instead of after the first long interval.
const claudeAutoInstallPending = 30 * time.Second

// autoInstallClaudeExporter installs the Claude Code usage exporter in every
// Claude profile the Worker knows, once usage monitoring is accepted. It
// reuses installClaudeUsage, which is idempotent, keeps the previous status
// line and refuses a setup it cannot keep. A profile under managed settings is
// never touched. The outcome is reported through the harness scan. It reports
// pending when a profile waits for enrollment or for the usage consent.
func autoInstallClaudeExporter(ctx context.Context, res *config.Resolver, stateDir string, lookup func(string) (string, bool)) (pending bool) {
	// Fail closed: only a policy that resolved to "auto" allows an edit. An
	// invalid or unreadable policy does nothing and reports a conflict.
	if res == nil {
		agent.SetClaudeExporterIssue(ops.UsageExporterConflict)
		return false
	}
	v, err := res.Resolve(config.KeyUsageClaudeExporter)
	switch {
	case err != nil:
		agent.SetClaudeExporterIssue(ops.UsageExporterConflict)
		return false
	case v.Value != "auto":
		agent.SetClaudeExporterIssue(ops.UsageExporterOff)
		return false
	}
	agent.SetClaudeExporterIssue("")
	if agent.ClaudeManagedOverrides(agent.ClaudeManagedSettingsFile) {
		agent.SetClaudeExporterIssue(ops.UsageExporterOverridden)
		return false
	}
	binary, err := os.Executable()
	if err != nil {
		return false
	}
	shell, powerShell, err := claudeUsageShell(lookup)
	if err != nil {
		return false
	}
	for _, dir := range agent.ClaudeProfileDirs() {
		if _, err := os.Stat(dir); err != nil {
			continue // no Claude Code profile here
		}
		integration := claudeUsageIntegration{Version: 1, Binary: binary, StateDir: stateDir, Shell: shell, PowerShell: powerShell}
		_, err := installClaudeUsage(ctx, dir, integration, agent.CurrentClaudeSubscription)
		switch {
		case errors.Is(err, errClaudeNotEnrolled), errors.Is(err, errClaudeNotAccepted):
			pending = true
		case err == nil, ctx.Err() != nil:
		case errors.Is(err, errClaudeHooksDisabled), errors.Is(err, errClaudeManaged):
			agent.SetClaudeExporterIssue(ops.UsageExporterOverridden)
		default:
			agent.SetClaudeExporterIssue(ops.UsageExporterConflict)
		}
	}
	return pending
}

// runClaudeAutoInstall runs autoInstallClaudeExporter at the start and then
// periodically until ctx ends: every 30 seconds while it waits for the
// enrollment or the usage consent, then every 10 minutes.
//
// resolver returns the policy resolver and is called before every pass, so
// that a change of usage_claude_exporter (also "off", saved to remove the
// integration) applies without a restart.
func runClaudeAutoInstall(ctx context.Context, resolver func() *config.Resolver, stateDir string, lookup func(string) (string, bool)) {
	for {
		wait := claudeAutoInstallEvery
		if autoInstallClaudeExporter(ctx, resolver(), stateDir, lookup) {
			wait = claudeAutoInstallPending
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
