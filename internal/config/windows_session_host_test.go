package config

import (
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWindowsSessionHostSelection(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	env := func(vars map[string]string) PathEnv {
		return PathEnv{GOOS: "windows", Getenv: func(k string) string { return vars[k] },
			HomeDir: func() (string, error) { return home, nil }}
	}
	found := func(string) (string, error) { return `C:\psmux\psmux.exe`, nil }
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	def := filepath.Join(home, ".psmux", "default")
	custom := filepath.Join(t.TempDir(), "sock")
	data := filepath.Join(t.TempDir(), "psdata")
	for _, tc := range []struct {
		name, mode, configured string
		vars                   map[string]string
		look                   func(string) (string, error)
		want                   string
	}{
		{"conpty never uses psmux", WindowsHostConPTY, "", nil, found, ""},
		{"auto without psmux is ConPTY", WindowsHostAuto, "", nil, missing, ""},
		{"auto with psmux is the default psmux server", WindowsHostAuto, "", nil, found, def},
		{"psmux even when not found (fails loudly later)", WindowsHostPsmux, "", nil, missing, def},
		{"configured socket wins", WindowsHostAuto, custom, nil, found, custom},
		{"PSMUX_DATA_DIR moves the default", WindowsHostPsmux, "", map[string]string{"PSMUX_DATA_DIR": data}, missing, filepath.Join(data, "default")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := windowsSocket(tc.mode, tc.configured, env(tc.vars), tc.look); got != tc.want {
				t.Fatalf("socket = %q, want %q", got, tc.want)
			}
		})
	}
	noHome := PathEnv{GOOS: "windows", Getenv: func(string) string { return "" }, HomeDir: func() (string, error) { return "", errors.New("no home") }}
	if got := windowsSocket(WindowsHostPsmux, "", noHome, found); got != "" {
		t.Fatalf("no home: socket = %q, want ConPTY", got)
	}
}

// On a real Windows machine with psmux on PATH (the CI psmux job), the default
// resolution picks psmux.
func TestWindowsSessionHostAutoOnThisMachine(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows only")
	}
	if _, err := exec.LookPath("psmux"); err != nil {
		t.Skip("psmux is not installed")
	}
	if got := windowsSocket(WindowsHostAuto, "", SystemPathEnv(), exec.LookPath); got == "" {
		t.Fatal("auto did not pick psmux although it is on PATH")
	}
}
