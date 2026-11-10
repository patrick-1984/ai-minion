package config

import (
	"errors"
	"path/filepath"
	"testing"
)

func fakeEnv(goos string, env map[string]string) PathEnv {
	return PathEnv{
		GOOS:    goos,
		Getenv:  func(k string) string { return env[k] },
		HomeDir: func() (string, error) { return filepath.FromSlash("/home/u"), nil },
		TempDir: func() string { return filepath.FromSlash("/tmp") },
		UID:     1000,
	}
}

func TestPathsPerOS(t *testing.T) {
	fs := filepath.FromSlash
	cases := []struct {
		name                string
		goos                string
		env                 map[string]string
		data, state, runtim string
	}{
		{"linux defaults", "linux", nil,
			fs("/home/u/.local/share/minion"), fs("/home/u/.local/state/minion"), fs("/tmp/minion-1000")},
		{"linux XDG", "linux",
			map[string]string{"XDG_DATA_HOME": fs("/x/data"), "XDG_STATE_HOME": fs("/x/state"), "XDG_RUNTIME_DIR": fs("/run/user/1000")},
			fs("/x/data/minion"), fs("/x/state/minion"), fs("/run/user/1000/minion")},
		{"linux relative XDG ignored", "linux",
			map[string]string{"XDG_DATA_HOME": "rel", "XDG_STATE_HOME": "rel", "XDG_RUNTIME_DIR": "rel"},
			fs("/home/u/.local/share/minion"), fs("/home/u/.local/state/minion"), fs("/tmp/minion-1000")},
		{"macOS", "darwin", map[string]string{"XDG_STATE_HOME": fs("/ignored")},
			fs("/home/u/Library/Application Support/minion"), fs("/home/u/Library/Application Support/minion"), fs("/tmp/minion-1000")},
		{"windows LOCALAPPDATA", "windows", map[string]string{"LOCALAPPDATA": fs("/c/Local")},
			fs("/c/Local/minion/data"), fs("/c/Local/minion/state"), fs("/c/Local/minion/run")},
		{"windows fallback", "windows", nil,
			fs("/home/u/AppData/Local/minion/data"), fs("/home/u/AppData/Local/minion/state"), fs("/home/u/AppData/Local/minion/run")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := fakeEnv(c.goos, c.env)
			for _, x := range []struct {
				what string
				fn   func() (string, error)
				want string
			}{{"data", p.DataDir, c.data}, {"state", p.StateDir, c.state}, {"runtime", p.RuntimeDir, c.runtim}} {
				got, err := x.fn()
				if err != nil || got != x.want {
					t.Errorf("%s = %q (err %v), want %q", x.what, got, err, x.want)
				}
			}
		})
	}
}

func TestPathsHomeErrors(t *testing.T) {
	p := fakeEnv("linux", nil)
	p.HomeDir = func() (string, error) { return "", errors.New("no home") }
	if _, err := p.DataDir(); err == nil {
		t.Error("expected error without home")
	}
	p.HomeDir = func() (string, error) { return "", nil }
	if _, err := p.StateDir(); err == nil {
		t.Error("expected error for empty home")
	}
	// An absolute XDG variable needs no home.
	p = fakeEnv("linux", map[string]string{"XDG_DATA_HOME": filepath.FromSlash("/x")})
	p.HomeDir = func() (string, error) { return "", errors.New("no home") }
	if _, err := p.DataDir(); err != nil {
		t.Errorf("XDG without home: %v", err)
	}
}

func TestSystemPathEnv(t *testing.T) {
	p := SystemPathEnv()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if p.GOOS == "" || p.Getenv == nil || p.HomeDir == nil || p.TempDir == nil {
		t.Fatalf("incomplete %+v", p)
	}
}
