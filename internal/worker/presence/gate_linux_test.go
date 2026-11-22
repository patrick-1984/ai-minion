//go:build linux

package presence

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The systemd registrations and the XDG state directory are Linux conventions:
// these tests do not run elsewhere.

func TestServiceStateDirs(t *testing.T) {
	home := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(home, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(p, 0o644)
	}
	write(".config/systemd/user/minion-worker.service", "[Service]\nEnvironment=\"MINION_STATE_DIR=/srv/my state\"\nExecStart=/x\n")
	write(".config/systemd/user/minion-worker.service.d/10.conf", "[Service]\nEnvironment=FOO=1 XDG_STATE_HOME=/var/svc-state\nEnvironment=XDG_STATE_HOME=%h/x\n")
	write(".config/minion/worker-login", "/usr/bin/minion\n/home/me/login-state\n")
	got := serviceStateDirs(home, os.Getuid())
	want := []string{"/srv/my state", "/var/svc-state/minion", "/home/me/login-state"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v want %v", got, want)
	}
	// A group-writable registration is not trusted.
	_ = os.Chmod(filepath.Join(home, ".config/minion/worker-login"), 0o666)
	if got := serviceStateDirs(home, os.Getuid()); len(got) != 2 {
		t.Fatalf("untrusted file read: %v", got)
	}
}

func TestReaderFindsAServiceWithItsOwnXDGStateHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "login-xdg")) // the fresh login
	t.Setenv("MINION_STATE_DIR", "")
	svcState := filepath.Join(home, "svc-xdg", "minion")
	if err := os.MkdirAll(svcState, 0o700); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(home, ".config/systemd/user")
	_ = os.MkdirAll(unit, 0o755)
	if err := os.WriteFile(filepath.Join(unit, "minion-worker.service"), []byte("[Service]\nEnvironment=XDG_STATE_HOME="+filepath.Join(home, "svc-xdg")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Join(unit, "minion-worker.service"), 0o644)
	// The service publishes without a pointer (an older release).
	l := Layout{Scope: ScopeUser, UID: me(), StateDir: svcState, PrivateDir: filepath.Join(svcState, "presence")}
	clk := newClock()
	start(t, newPub(t, l, "svc", clk))
	r := NewReader()
	r.PublicRoot = ""
	r.Now = clk.Now
	if res := Best(r.Scan()); res.Status != StatusRunning || res.Candidate.Instance != "svc" {
		t.Fatalf("%+v %v", res, res.Err)
	}
}

// A registration path that is a FIFO must not hang discovery: it is opened
// without blocking and refused before anything is read.
func TestServiceRegistrationThatIsAFIFOIsSkipped(t *testing.T) {
	home := t.TempDir()
	unit := filepath.Join(home, ".config", "systemd", "user", "minion-worker.service")
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(unit, 0o644); err != nil {
		t.Skipf("no FIFO: %v", err)
	}
	login := filepath.Join(home, ".config", "minion", "worker-login")
	if err := os.MkdirAll(filepath.Dir(login), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(login, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan []string, 1)
	go func() { done <- serviceStateDirs(home, os.Getuid()) }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Fatalf("read a FIFO: %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serviceStateDirs blocks on a FIFO registration")
	}
}
