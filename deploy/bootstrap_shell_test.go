//go:build !windows

// Package deploy holds the deployment files; this test runs the shell harness of
// bootstrap.sh so that CI exercises the artifact-trust regressions (a forged or
// tampered cache is refused, the executed bytes come from a proven archive).
package deploy

import (
	"os/exec"
	"testing"
)

func TestBootstrapShellHarness(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	for _, tool := range []string{"sh", "curl", "tar", "awk", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	if _, err := exec.LookPath("busybox"); err != nil {
		if _, err := exec.LookPath("python3"); err != nil {
			t.Skip("neither busybox nor python3 is installed to serve the fake release")
		}
	}
	out, err := exec.Command("sh", "bootstrap_test.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("bootstrap_test.sh failed: %v\n%s", err, out)
	}
}
