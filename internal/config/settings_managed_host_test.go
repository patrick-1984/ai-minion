package config

import (
	"runtime"
	"strings"
	"testing"
)

func TestManagedHostDefaultIsOrdinary(t *testing.T) {
	m, err := ResolveManagedHost(workerResolver(nil, nil, nil))
	if err != nil || m != (ManagedHost{}) {
		t.Fatalf("%+v, %v", m, err)
	}
	if m, err := ResolveManagedHost(nil); err != nil || m != (ManagedHost{}) {
		t.Fatalf("nil resolver: %+v, %v", m, err)
	}
}

func TestManagedHostFromTheEnvironment(t *testing.T) {
	skipWindowsPaths(t)
	env := map[string]string{
		"MINION_MANAGED_HOST": "android",
		"MINION_TMUX_BINARY":  "/data/app/x/lib/arm64/libtmux.so",
		"MINION_TMUX_CONFIG":  "/data/user/0/app/files/tmux.conf",
	}
	m, err := ResolveManagedHost(workerResolver(nil, env, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !m.Android || m.TmuxBinary != env["MINION_TMUX_BINARY"] || m.TmuxConfig != env["MINION_TMUX_CONFIG"] {
		t.Errorf("m = %+v", m)
	}
}

func TestManagedHostRefusals(t *testing.T) {
	skipWindowsPaths(t)
	cases := map[string]map[string]string{
		"android needs a binary": {"MINION_MANAGED_HOST": "android"},
		"relative binary":        {"MINION_TMUX_BINARY": "tmux"},
		"relative config":        {"MINION_MANAGED_HOST": "android", "MINION_TMUX_BINARY": "/x/tmux", "MINION_TMUX_CONFIG": "t.conf"},
		"unknown mode":           {"MINION_MANAGED_HOST": "ios"},
	}
	for name, env := range cases {
		if _, err := ResolveManagedHost(workerResolver(nil, env, nil)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The Android mode never uses the default socket: the private socket must be
// named, so that no default of the user can be reached by accident.
func TestWorkerCoreAndroidNeedsAPrivateSocket(t *testing.T) {
	skipWindowsPaths(t)
	dir := t.TempDir()
	env := map[string]string{"MINION_STATE_DIR": dir, "MINION_MANAGED_HOST": "android", "MINION_TMUX_BINARY": "/x/tmux"}
	if _, err := ResolveWorkerCore(workerResolver(nil, env, nil)); err == nil || !strings.Contains(err.Error(), "tmux_socket") {
		t.Fatalf("err = %v", err)
	}
	env["MINION_TMUX_SOCKET"] = "/data/run/tmux.sock"
	wc, err := ResolveWorkerCore(workerResolver(nil, env, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !wc.Managed.Android || wc.TmuxSocket != "/data/run/tmux.sock" {
		t.Errorf("core = %+v", wc)
	}
}

// The Android paths of these tests are slash paths.
func skipWindowsPaths(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Android paths are not absolute paths on Windows")
	}
}

func TestManagedHostIgnoresSavedSupervisorOverrides(t *testing.T) {
	file := MapSource{
		KeyManagedHost: ManagedHostAndroid,
		KeyTmuxBinary:  "/tmp/not-a-trusted-binary",
		KeyTmuxConfig:  "/tmp/not-a-trusted-config",
	}
	host, err := ResolveManagedHost(workerResolver(nil, nil, file))
	if err != nil || host != (ManagedHost{}) {
		t.Fatalf("saved supervisor settings affected the host: %+v, %v", host, err)
	}
}
