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

// autoInstallClaudeExporter installs the Claude Code usage exporter in every
// Claude profile the Worker knows, once usage monitoring is accepted. It
// reuses installClaudeUsage, which is idempotent, keeps the previous status
// line and refuses a setup it cannot keep. A profile under managed settings is
// never touched. The outcome is reported through the harness scan.
func autoInstallClaudeExporter(ctx context.Context, res *config.Resolver, stateDir string, lookup func(string) (string, bool)) {
	if v, err := res.Resolve(config.KeyUsageClaudeExporter); err == nil && v.Value == "off" {
		agent.SetClaudeExporterIssue(ops.UsageExporterOff)
		return
	}
	agent.SetClaudeExporterIssue("")
	if agent.ClaudeManagedOverrides(agent.ClaudeManagedSettingsFile) {
		agent.SetClaudeExporterIssue(ops.UsageExporterOverridden)
		return
	}
	binary, err := os.Executable()
	if err != nil {
		return
	}
	shell, powerShell, err := claudeUsageShell(lookup)
	if err != nil {
		return
	}
	for _, dir := range agent.ClaudeProfileDirs() {
		if _, err := os.Stat(dir); err != nil {
			continue // no Claude Code profile here
		}
		integration := claudeUsageIntegration{Version: 1, Binary: binary, StateDir: stateDir, Shell: shell, PowerShell: powerShell}
		_, err := installClaudeUsage(ctx, dir, integration, agent.CurrentClaudeSubscription)
		switch {
		case err == nil, errors.Is(err, errClaudeNotEnrolled), errors.Is(err, errClaudeNotAccepted), ctx.Err() != nil:
		case errors.Is(err, errClaudeHooksDisabled):
			agent.SetClaudeExporterIssue(ops.UsageExporterOverridden)
		default:
			agent.SetClaudeExporterIssue(ops.UsageExporterConflict)
		}
	}
}

// runClaudeAutoInstall runs autoInstallClaudeExporter at the start and then
// periodically until ctx ends.
func runClaudeAutoInstall(ctx context.Context, res *config.Resolver, stateDir string, lookup func(string) (string, bool)) {
	for {
		autoInstallClaudeExporter(ctx, res, stateDir, lookup)
		select {
		case <-ctx.Done():
			return
		case <-time.After(claudeAutoInstallEvery):
		}
	}
}
