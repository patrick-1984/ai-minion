//go:build !windows

// Package deploy holds the deployment files; this test runs the shell harness of
// bootstrap.sh so that CI exercises the artifact-trust regressions (a forged or
// tampered cache is refused, the executed bytes come from a proven archive).
package deploy

import (
	"os"
	"os/exec"
	"strings"
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

// TestInstallShellHarness runs the tests of install.sh, the one-line installer.
func TestInstallShellHarness(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	for _, tool := range []string{"sh", "curl", "tar", "awk", "sed"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	out, err := exec.Command("sh", "install_test.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("install_test.sh failed: %v\n%s", err, out)
	}
}

// TestDeviceInstallShellHarness verifies asset selection and write boundaries
// without requiring an Android device or an ESP32.
func TestDeviceInstallShellHarness(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	for _, tool := range []string{"sh", "curl", "sha256sum", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	out, err := exec.Command("sh", "device_install_test.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("device_install_test.sh failed: %v\n%s", err, out)
	}
}

// install.ps1 -Join takes the token from MINION_JOIN (and clears it) before it
// falls back to the hidden prompt. No PowerShell runs in this test.
func TestInstallPS1ReadsTheJoinTokenFromTheEnvironment(t *testing.T) {
	b, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	env := strings.Index(s, "$env:MINION_JOIN")
	clear := strings.Index(s, "Remove-Item Env:MINION_JOIN")
	prompt := strings.Index(s, "Read-Host")
	check := strings.Index(s, "'^mnj1-")
	if env < 0 || clear < env || prompt < clear || check < prompt {
		t.Fatalf("install.ps1 must read and clear MINION_JOIN, then prompt, then validate (%d %d %d %d)", env, clear, prompt, check)
	}
}
