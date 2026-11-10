package service

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

type fakeController struct {
	calls     []string
	statuses  []Status
	installEr error
	startErr  error
}

func (f *fakeController) Install() error   { f.calls = append(f.calls, "install"); return f.installEr }
func (f *fakeController) Start() error     { f.calls = append(f.calls, "start"); return f.startErr }
func (f *fakeController) Uninstall() error { f.calls = append(f.calls, "uninstall"); return nil }
func (f *fakeController) Status() (Status, error) {
	f.calls = append(f.calls, "status")
	if len(f.statuses) == 0 {
		return StatusStopped, nil
	}
	s := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	return s, nil
}

func TestKardianosConfigRunsTheSCMEntryPoint(t *testing.T) {
	c := KardianosConfig(`C:\Program Files\minion\minion.exe`)
	if c.Name != WindowsServiceName || c.Executable != `C:\Program Files\minion\minion.exe` ||
		strings.Join(c.Arguments, " ") != "worker "+WindowsServiceCommand {
		t.Fatalf("%+v", c)
	}
}

func TestNewKardianosControllerBuilds(t *testing.T) {
	// The platform service manager exists on every supported OS, and a
	// platform without one reports an error instead of panicking.
	if c, err := NewKardianosController("/usr/bin/minion"); err == nil && c == nil {
		t.Fatal("nil controller without an error")
	}
}

func TestInstallControllerWindowsFlow(t *testing.T) {
	fc := &fakeController{statuses: []Status{StatusStopped, StatusRunning}}
	d := testDeps(t, nil)
	d.NewController = func(string) (Controller, error) { return fc, nil }
	res, err := Install(context.Background(), d, Request{Manager: ManagerWindows, Scope: ScopeSystem, Binary: `C:\minion.exe`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fc.calls, ","); !strings.HasPrefix(got, "uninstall,install,start,status") {
		t.Errorf("calls = %s", got)
	}
	if res.Unit != WindowsServiceName {
		t.Errorf("result = %+v", res)
	}
}

func TestInstallControllerErrors(t *testing.T) {
	d := testDeps(t, nil)
	for name, fc := range map[string]*fakeController{
		"install": {installEr: errors.New("access denied")},
		"start":   {startErr: errors.New("cannot start")},
		"stopped": {statuses: []Status{StatusStopped}},
	} {
		d.NewController = func(string) (Controller, error) { return fc, nil }
		if _, err := Install(context.Background(), d, Request{Manager: ManagerWindows, Binary: `C:\minion.exe`}, nil); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	d.NewController = func(string) (Controller, error) { return nil, errors.New("no scm") }
	if _, err := Install(context.Background(), d, Request{Manager: ManagerWindows, Binary: "x"}, nil); err == nil {
		t.Error("controller error ignored")
	}
	if _, err := Install(context.Background(), d, Request{Manager: ManagerWindows}, nil); err == nil {
		t.Error("empty binary accepted")
	}
}

func TestExecRunnerReturnsOutputAndError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	out, err := ExecRunner{}.Run(context.Background(), "sh", "-c", "echo hi; exit 3")
	if err == nil || strings.TrimSpace(out) != "hi" {
		t.Fatalf("out = %q err = %v", out, err)
	}
}

func TestResultDescribe(t *testing.T) {
	s := Result{Manager: ManagerSystemd, Scope: ScopeSystem, Unit: "u.service", Path: "/p"}.Describe()
	if !strings.Contains(s, "u.service") || !strings.Contains(s, "/p") {
		t.Fatal(s)
	}
}

func TestSleepHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	Deps{}.sleep(ctx, 1<<62) // returns at once
}

func TestInstallWithMissingLingerDoesNotClaimItStartsAtBoot(t *testing.T) {
	r := &fakeRunner{answer: func(line string) (string, error) {
		switch {
		case strings.Contains(line, "is-active"):
			return "active\n", nil
		case strings.HasPrefix(line, "loginctl show-user"):
			return "Linger=no\n", nil
		case strings.HasPrefix(line, "loginctl enable-linger"):
			return "denied", errors.New("exit status 1")
		}
		return "", nil
	}}
	d := testDeps(t, r)
	req := Request{Manager: ManagerSystemd, Scope: ScopeUser, Binary: "/usr/bin/minion",
		Target: Target{User: "ann", UID: 1000, GID: 1000, Home: t.TempDir()}}
	var out strings.Builder
	_, err := Install(context.Background(), d, req, &out)
	var le *LingerError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(out.String(), "starts at boot") || !strings.Contains(out.String(), "stops when you log out") {
		t.Errorf("report: %q", out.String())
	}
}
