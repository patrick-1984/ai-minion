//go:build unix

package cli

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
)

func TestLiveControlSocketFindsARunningWorkerAndIgnoresAStaleSocket(t *testing.T) {
	// A short path: unix socket paths are limited to about 100 bytes.
	run, err := os.MkdirTemp("", "mnr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(run) })
	env := config.PathEnv{
		GOOS:    "linux",
		Getenv:  func(k string) string { return map[string]string{"XDG_RUNTIME_DIR": run}[k] },
		TempDir: func() string { return run }, UID: 4242,
		HomeDir: func() (string, error) { return run, nil },
	}
	if sock, ok := liveControlSocket(env, time.Second); ok {
		t.Fatalf("found a Worker at %s before one runs", sock)
	}
	dir := filepath.Join(run, "minion")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "worker.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if sock, ok := liveControlSocket(env, time.Second); !ok || sock != path {
		t.Errorf("live socket = %q, %v; want %s", sock, ok, path)
	}
	// The Worker dies and leaves its socket file behind: nobody answers.
	if ul, ok := l.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = l.Close()
	if sock, ok := liveControlSocket(env, time.Second); ok {
		t.Errorf("a stale socket %s counted as a running Worker", sock)
	}
}
