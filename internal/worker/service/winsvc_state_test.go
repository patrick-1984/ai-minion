package service

import (
	"strings"
	"testing"
)

func TestForegroundServiceWarning(t *testing.T) {
	stopDelete := []string{"sc.exe stop MinionWorker, then sc.exe delete MinionWorker"}
	deleteOnly := []string{"sc.exe delete MinionWorker", "sc.exe config MinionWorker start= demand"}
	for _, tc := range []struct {
		st    WindowsServiceState
		want  string
		fixes []string
	}{
		{WindowsServiceState{}, "", nil},
		{WindowsServiceState{Installed: true}, "", nil},
		{WindowsServiceState{Installed: true, Running: true, Account: "LocalSystem"}, "is also installed and running", stopDelete},
		{WindowsServiceState{Installed: true, AutoStart: true}, "installed and set to start at boot", deleteOnly},
		{WindowsServiceState{Installed: true, Running: true, AutoStart: true, Account: `ASUS\pat`}, `runs as ASUS\pat`, stopDelete},
	} {
		got := ForegroundServiceWarning(tc.st)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("ForegroundServiceWarning(%+v) = %q, want %q", tc.st, got, tc.want)
		}
		if got == "" {
			continue
		}
		// worker down would stop the foreground Worker once the service is
		// stopped: the warning must never send the operator there.
		if strings.Contains(got, "worker down") || !strings.Contains(got, "hidden ConPTY") {
			t.Errorf("warning = %q", got)
		}
		for _, f := range tc.fixes {
			if !strings.Contains(got, f) {
				t.Errorf("warning %q lacks %q", got, f)
			}
		}
		if !tc.st.Running && strings.Contains(got, "sc.exe stop") {
			t.Errorf("a stopped service is told to stop: %q", got)
		}
	}
}
