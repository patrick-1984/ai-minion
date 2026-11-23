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
)

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
