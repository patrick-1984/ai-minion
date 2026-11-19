package service

import (
	"errors"
	"strings"
	"testing"
)

func TestRefuseSystem(t *testing.T) {
	if err := RefuseSystem(false); err != nil {
		t.Error(err)
	}
	if err := RefuseSystem(true); !errors.Is(err, ErrRunsAsSystem) {
		t.Errorf("err = %v", err)
	}
}

func TestLegacyQueryFailureNamesTheTrustedRemoval(t *testing.T) {
	m := LegacyQueryFailure(errors.New("Access is denied"))
	if !strings.Contains(m, "Access is denied") || !strings.Contains(m, "sc.exe delete MinionWorker") || strings.Contains(m, "minion.exe") {
		t.Error(m)
	}
}

func TestSCMEndRunsTheHandlerInAConsoleAndUnderTheManager(t *testing.T) {
	n := 0
	if err := (SCM{GOOS: "linux"}).End(func() { n++ }); err != nil || n != 1 {
		t.Fatal(err, n)
	}
	under := SCM{GOOS: "windows", Interactive: func() bool { return false }, EndUnderSCM: func(fn func()) error { fn(); return nil }}
	if err := under.End(func() { n++ }); err != nil || n != 2 {
		t.Fatal(err, n)
	}
}

// The removal command disables the service before it stops it, checks the
// stop, deletes it and checks it is gone, in that order, and aborts on an
// error.
func TestLegacyRemovalCommandOrder(t *testing.T) {
	c := LegacyRemovalCommand
	last := -1
	for _, part := range []string{"ErrorActionPreference='Stop'", "-StartupType Disabled", "Stop-Service MinionWorker -Force", "-ne 'Stopped'", "sc.exe delete MinionWorker", "Get-Service MinionWorker -ErrorAction SilentlyContinue"} {
		i := strings.Index(c, part)
		if i <= last {
			t.Fatalf("%q is missing or out of order in:\n%s", part, c)
		}
		last = i
	}
	if strings.Contains(c, "minion.exe") {
		t.Error("the removal must not run a minion.exe")
	}
}
