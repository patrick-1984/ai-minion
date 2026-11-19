package service

import (
	"errors"
	"strings"
	"testing"
)

func TestAutostartCommandQuotesLikeWindows(t *testing.T) {
	for _, c := range []struct {
		exe  string
		args []string
		want string
	}{
		{`C:\Users\ann\AppData\Local\Programs\minion\minion.exe`, nil,
			`C:\Users\ann\AppData\Local\Programs\minion\minion.exe worker launch`},
		{`C:\Users\Ann Lee\minion\minion.exe`, []string{"--state-dir", `D:\minion state\`},
			`"C:\Users\Ann Lee\minion\minion.exe" worker launch --state-dir "D:\minion state\\"`},
		{`C:\m.exe`, []string{`say "hi"`, ""}, `C:\m.exe worker launch "say \"hi\"" ""`},
	} {
		got, err := AutostartCommand(c.exe, c.args)
		if err != nil || got != c.want {
			t.Errorf("AutostartCommand(%q, %q) = %q, %v; want %q", c.exe, c.args, got, err, c.want)
		}
	}
}

// Windows runs a Run value of at most 260 characters.
func TestAutostartCommandRefusesWhatWindowsWouldNotRun(t *testing.T) {
	if _, err := AutostartCommand(`C:\`+strings.Repeat("x", 300)+`\minion.exe`, nil); !errors.Is(err, ErrAutostartTooLong) {
		t.Errorf("err = %v", err)
	}
	if _, err := AutostartCommand(" ", nil); err == nil {
		t.Error("an empty path was accepted")
	}
}

func TestLegacyServiceIsTheLocalSystemOne(t *testing.T) {
	for _, c := range []struct {
		st   WindowsServiceState
		want bool
	}{
		{WindowsServiceState{}, false},
		{WindowsServiceState{Installed: true, Account: "LocalSystem"}, true},
		{WindowsServiceState{Installed: true, Account: `.\LocalSystem`}, true},
		{WindowsServiceState{Installed: true, Account: `NT AUTHORITY\SYSTEM`}, true},
		{WindowsServiceState{Installed: true}, true},
		{WindowsServiceState{Installed: true, Account: `.\ann`}, false},
	} {
		if got := IsLegacyService(c.st); got != c.want {
			t.Errorf("IsLegacyService(%+v) = %v", c.st, got)
		}
	}
	w := LegacyServiceWarning(WindowsServiceState{Installed: true, Running: true, Account: "LocalSystem"})
	for _, want := range []string{"LocalSystem", "installed and running", "Stop-Service MinionWorker", "sc.exe delete MinionWorker", "runs as you"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning lacks %q: %s", want, w)
		}
	}
	// The removal never asks to run a minion.exe elevated.
	if strings.Contains(strings.ToLower(LegacyRemovalCommand), "minion.exe") || strings.Contains(LegacyRemovalCommand, "minion worker") {
		t.Errorf("removal command = %s", LegacyRemovalCommand)
	}
	if LegacyServiceWarning(WindowsServiceState{}) != "" {
		t.Error("a warning without a service")
	}
}
