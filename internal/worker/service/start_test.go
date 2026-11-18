package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The Windows start asks for the status and starts; it never installs.
func TestWorkerStartNeverInstalls(t *testing.T) {
	fc := &fakeController{statuses: []Status{StatusStopped}}
	res, err := Deps{}.StartInstalled(context.Background(), StartRequest{GOOS: "windows", Windows: func() (Controller, error) { return fc, nil }})
	if err != nil || res.Manager != ManagerWindows || res.AlreadyRunning {
		t.Fatalf("start = %+v, %v", res, err)
	}
	if !slices.Equal(fc.calls, []string{"status", "start"}) {
		t.Fatalf("controller calls = %v, want only status and start", fc.calls)
	}
	// Running already: only the status.
	fc = &fakeController{statuses: []Status{StatusRunning}}
	if res, err := (Deps{}).StartInstalled(context.Background(), StartRequest{GOOS: "windows", Windows: func() (Controller, error) { return fc, nil }}); err != nil || !res.AlreadyRunning || !slices.Equal(fc.calls, []string{"status"}) {
		t.Fatalf("running: %+v %v %v", res, err, fc.calls)
	}
	// Access denied: needs admin with the command.
	fc = &fakeController{startErr: fmt.Errorf("start: %w", os.ErrPermission)}
	_, err = Deps{}.StartInstalled(context.Background(), StartRequest{GOOS: "windows", Windows: func() (Controller, error) { return fc, nil }})
	var na *NeedsAdminError
	if !errors.As(err, &na) || !strings.Contains(na.Hint, "Start-Service") {
		t.Fatalf("denied = %v", err)
	}
}

type notInstalledController struct{ fakeController }

func (c *notInstalledController) Status() (Status, error) {
	c.calls = append(c.calls, "status")
	return StatusUnknown, ErrNotInstalled
}

func TestWorkerStartNotInstalled(t *testing.T) {
	c := &notInstalledController{}
	if _, err := (Deps{}).StartInstalled(context.Background(), StartRequest{GOOS: "windows", Windows: func() (Controller, error) { return c, nil }}); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("windows = %v", err)
	}
	r := &fakeRunner{answer: systemctlAnswers(nil)}
	if _, err := testDeps(t, r).StartInstalled(context.Background(), StartRequest{GOOS: "linux", User: "ann", Home: t.TempDir(), EUID: 1000}); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("linux template only = %v", err)
	}
	for _, l := range r.lines() {
		if strings.Contains(l, " start ") || strings.Contains(l, " enable ") || strings.Contains(l, "daemon-reload") {
			t.Fatalf("ran %q", l)
		}
	}
	if _, err := (Deps{Runner: &fakeRunner{}}).StartInstalled(context.Background(), StartRequest{GOOS: "darwin", Home: t.TempDir()}); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("darwin without plist = %v", err)
	}
}

func TestWorkerStartSystemd(t *testing.T) {
	sys := SystemUnitFor("ann")
	// System instance as non-root: needs admin, the hint is sudo.
	r := &fakeRunner{answer: systemctlAnswers(map[string]string{"systemctl is-enabled " + sys: "enabled"})}
	_, err := testDeps(t, r).StartInstalled(context.Background(), StartRequest{GOOS: "linux", User: "ann", Home: t.TempDir(), EUID: 1000})
	var na *NeedsAdminError
	if !errors.As(err, &na) || na.Hint != "sudo systemctl start "+sys {
		t.Fatalf("system as non-root = %v", err)
	}
	// As root it starts the system unit.
	r = &fakeRunner{answer: systemctlAnswers(map[string]string{"systemctl is-enabled " + sys: "enabled"})}
	res, err := testDeps(t, r).StartInstalled(context.Background(), StartRequest{GOOS: "linux", User: "ann", Home: t.TempDir(), EUID: 0})
	if err != nil || res.Unit != sys || r.count("systemctl start "+sys) != 1 {
		t.Fatalf("root = %+v %v %v", res, err, r.lines())
	}
	// A user unit starts in the user manager.
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".config", "systemd", "user", UserUnitName))
	r = &fakeRunner{answer: systemctlAnswers(nil)}
	res, err = testDeps(t, r).StartInstalled(context.Background(), StartRequest{GOOS: "linux", User: "ann", Home: home, EUID: 1000})
	if err != nil || res.Scope != ScopeUser || r.count("systemctl --user start "+UserUnitName) != 1 {
		t.Fatalf("user = %+v %v %v", res, err, r.lines())
	}
}

func TestWorkerStartLaunchd(t *testing.T) {
	home := t.TempDir()
	writeFile(t, PlistPath(home))
	r := &fakeRunner{answer: func(line string) (string, error) {
		if strings.HasPrefix(line, "launchctl print") {
			return "", errors.New("not loaded")
		}
		return "", nil
	}}
	if _, err := (Deps{Runner: r}).StartInstalled(context.Background(), StartRequest{GOOS: "darwin", Home: home, UID: 501}); err != nil {
		t.Fatal(err)
	}
	want := []string{"launchctl print gui/501/" + LaunchdLabel, "launchctl bootstrap gui/501 " + PlistPath(home), "launchctl kickstart gui/501/" + LaunchdLabel}
	if !slices.Equal(r.lines(), want) {
		t.Fatalf("launchctl = %v, want %v (the plist is never written)", r.lines(), want)
	}
}
