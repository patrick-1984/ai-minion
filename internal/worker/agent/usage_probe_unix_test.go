//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func usageInventoryFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	oldManaged := ClaudeManagedSettingsFile
	ClaudeManagedSettingsFile = filepath.Join(dir, "managed-settings.json")
	t.Cleanup(func() { ClaudeManagedSettingsFile = oldManaged })
	oldIssue := ClaudeExporterIssue()
	SetClaudeExporterIssue("")
	t.Cleanup(func() { SetClaudeExporterIssue(oldIssue) })
	return dir
}

func writeUsageClaudeVersion(t *testing.T, dir, version string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho "+version+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestUsageCachedInventoryRefreshesClaudeExporter(t *testing.T) {
	dir := usageInventoryFixture(t)
	writeUsageClaudeVersion(t, dir, "1.2.3")

	var cache usageInventoryCache
	before, err := cache.get(t.Context(), "grant", false)
	if err != nil {
		t.Fatal(err)
	}
	if h := before.Inventory[0]; h.Name != "claude" || h.Exporter != ops.UsageExporterAbsent {
		t.Fatalf("first Claude inventory = %+v", h)
	}
	settings := `{"statusLine":{"type":"command","command":"minion usage _claude-export"}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := claudeExporterState(); got != ops.UsageExporterInstalled {
		t.Fatalf("Claude exporter state = %q, want installed", got)
	}
	writeUsageClaudeVersion(t, dir, "1.2.4")
	after, err := cache.get(t.Context(), "grant", false)
	if err != nil {
		t.Fatal(err)
	}
	if h := after.Inventory[0]; h.Exporter != ops.UsageExporterInstalled {
		t.Fatalf("cached report exporter = %q, want installed", h.Exporter)
	}
	if after.Inventory[0].Version != "1.2.3" || *after.InventoryAt != *before.InventoryAt || before.Inventory[0].Exporter != ops.UsageExporterAbsent {
		t.Fatal("refresh rescanned the inventory or modified the first report")
	}
	if err := os.Remove(filepath.Join(dir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	SetClaudeExporterIssue(ops.UsageExporterOff)
	removed, err := cache.get(t.Context(), "grant", false)
	if err != nil || removed.Inventory[0].Exporter != ops.UsageExporterOff {
		t.Fatalf("report after removing the exporter = %+v, %v", removed, err)
	}
}

func TestUsageForcedInventoryBypassesCacheWithinRateLimit(t *testing.T) {
	dir := usageInventoryFixture(t)
	var cache usageInventoryCache
	scan := func(fresh bool, wantVersion string) {
		t.Helper()
		report, err := cache.get(t.Context(), "grant", fresh)
		if err != nil {
			t.Fatal(err)
		}
		if got := report.Inventory[0].Version; got != wantVersion {
			t.Fatalf("fresh=%v: Claude version = %q, want %q", fresh, got, wantVersion)
		}
	}
	writeUsageClaudeVersion(t, dir, "1.2.3")
	scan(false, "1.2.3")
	writeUsageClaudeVersion(t, dir, "1.2.4")
	scan(false, "1.2.3")
	scan(true, "1.2.4")
	writeUsageClaudeVersion(t, dir, "1.2.5")
	scan(true, "1.2.4")
	scan(false, "1.2.4")

	// Advance the cooldown without expiring the five-minute cache.
	cache.nextForced = time.Now().Add(-time.Second)
	scan(true, "1.2.5")
}

// A harness whose version command leaves a background helper running must
// not leave that helper behind after the inventory scan.
func TestInventoryProbeLeavesNoBackgroundProcess(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "helper.pid")
	script := "#!/bin/sh\n(sleep 30) &\necho $! > " + pidFile + "\necho opencode 1.2.3\n"
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	var version string
	for _, h := range usageInventory(context.Background()) {
		if h.Name == "opencode" {
			version = h.Version
		}
	}
	if version != "1.2.3" {
		t.Fatalf("version %q", version)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the probe's background helper is still running after the scan")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
