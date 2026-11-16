package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsSessionHostSelection(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	env := func(vars map[string]string, homeErr bool) PathEnv {
		return PathEnv{GOOS: "windows", Getenv: func(k string) string { return vars[k] },
			HomeDir: func() (string, error) {
				if homeErr {
					return "", errors.New("no home")
				}
				return home, nil
			}}
	}
	custom := filepath.Join(t.TempDir(), "sock")
	data := filepath.Join(t.TempDir(), "psdata")
	profile := filepath.Join(t.TempDir(), "profile")
	for _, tc := range []struct {
		name, mode, configured string
		vars                   map[string]string
		homeErr                bool
		want                   string
	}{
		{"conpty is the default host", WindowsHostConPTY, "", nil, false, ""},
		{"conpty ignores a configured socket", WindowsHostConPTY, custom, nil, false, ""},
		{"psmux default server under the home", WindowsHostPsmux, "", nil, false, filepath.Join(home, ".psmux", "default")},
		{"USERPROFILE wins over the home lookup", WindowsHostPsmux, "", map[string]string{"USERPROFILE": profile}, true, filepath.Join(profile, ".psmux", "default")},
		{"configured socket wins", WindowsHostPsmux, custom, nil, false, custom},
		{"PSMUX_DATA_DIR moves the default", WindowsHostPsmux, "", map[string]string{"PSMUX_DATA_DIR": data}, true, filepath.Join(data, "default")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := windowsSocket(tc.mode, tc.configured, env(tc.vars, tc.homeErr))
			if err != nil || got != tc.want {
				t.Fatalf("socket = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	// psmux without any profile is an error, never a silent switch to ConPTY.
	if got, err := windowsSocket(WindowsHostPsmux, "", env(nil, true)); err == nil || !strings.Contains(err.Error(), "PSMUX_DATA_DIR") {
		t.Fatalf("no profile: %q, %v", got, err)
	}
}

func TestWindowsSessionHostDefaultIsConPTY(t *testing.T) {
	s, ok := Default.Lookup(KeyWindowsSessionHost)
	if !ok || s.Default != WindowsHostConPTY {
		t.Fatalf("default = %+v", s)
	}
}
