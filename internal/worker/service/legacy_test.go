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
