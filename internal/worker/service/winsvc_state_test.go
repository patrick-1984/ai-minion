package service

import (
	"strings"
	"testing"
)

func TestForegroundServiceWarning(t *testing.T) {
	for _, tc := range []struct {
		st   WindowsServiceState
		want string
	}{
		{WindowsServiceState{}, ""},
		{WindowsServiceState{Installed: true}, ""},
		{WindowsServiceState{Installed: true, Running: true, Account: "LocalSystem"}, "is also installed and running"},
		{WindowsServiceState{Installed: true, AutoStart: true}, "installed and set to start at boot"},
		{WindowsServiceState{Installed: true, Running: true, AutoStart: true, Account: `ASUS\pat`}, `runs as ASUS\pat`},
	} {
		got := ForegroundServiceWarning(tc.st)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("ForegroundServiceWarning(%+v) = %q, want %q", tc.st, got, tc.want)
		}
		if got != "" && (!strings.Contains(got, "minion worker down") || !strings.Contains(got, "sc.exe delete MinionWorker") || !strings.Contains(got, "hidden ConPTY")) {
			t.Errorf("warning lacks the commands: %q", got)
		}
	}
}
