package service

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

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
