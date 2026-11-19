package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// systemdAnswers is a fake systemctl: the listed command lines answer with
// their value, every other one is inactive (exit 3), as systemctl does.
func systemdAnswers(answers map[string]string) startRunner {
	return startRunner{answer: func(line string) (string, error) {
		if v, ok := answers[line]; ok {
			return v, nil
		}
		return "inactive", errors.New("exit status 3")
	}}
}

// The lifecycle block of status for each service manager, from the evidence
// the service manager gives (tray W10b; Astra finding #5: a packaged template
// alone is no installed Worker). The block never names a service binary.
func TestLifecycleBlockFixtures(t *testing.T) {
	type want struct {
		manager, scope, unit, hint  string
		installed, running          bool
		canStart, canStop, otherAcc bool
	}
	cases := []struct {
		name string
		goos string
		run  map[string]string
		// setup prepares the system unit directory and the home.
		setup  func(t *testing.T, unitDir, home string)
		win    *service.WindowsServiceState
		denied bool
		want   want
	}{
		{name: "systemd user", goos: "linux",
			run:  map[string]string{"systemctl --user is-active minion-worker.service": "active"},
			want: want{manager: "systemd", scope: "user", unit: "minion-worker.service", installed: true, running: true, canStop: true}},
		{name: "systemd system", goos: "linux",
			run:  map[string]string{"systemctl is-enabled minion-worker@ann.service": "enabled"},
			want: want{manager: "systemd", scope: "system", unit: "minion-worker@ann.service", installed: true, canStart: true, hint: "sudo systemctl start minion-worker@ann.service"}},
		{name: "systemd system of another account (denied socket)", goos: "linux", denied: true,
			run:  map[string]string{"systemctl is-active minion-worker@ann.service": "active"},
			want: want{manager: "systemd", scope: "system", unit: "minion-worker@ann.service", installed: true, running: true, otherAcc: true, hint: "sudo systemctl start minion-worker@ann.service"}},
		{name: "template only", goos: "linux",
			setup: func(t *testing.T, unitDir, _ string) {
				if err := os.WriteFile(filepath.Join(unitDir, service.TemplateUnitName), []byte("[Unit]\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: want{manager: "none"}},
		{name: "launchd", goos: "darwin",
			run: map[string]string{"launchctl print gui/1000/" + service.LaunchdLabel: "state = running"},
			setup: func(t *testing.T, _, home string) {
				p := service.PlistPath(home)
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("<plist/>"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: want{manager: "launchd", scope: "user", unit: service.LaunchdLabel, installed: true, running: true, canStop: true}},
		{name: "Windows stopped", goos: "windows", win: &service.WindowsServiceState{Installed: true},
			want: want{manager: "windows", scope: "system", unit: "MinionWorker", installed: true, canStart: true}},
		{name: "Windows running for another account", goos: "windows", win: &service.WindowsServiceState{Installed: true, Running: true},
			want: want{manager: "windows", scope: "system", unit: "MinionWorker", installed: true, running: true, otherAcc: true}},
		{name: "none", goos: "linux", want: want{manager: "none"}},
		{name: "none on Windows", goos: "windows", win: &service.WindowsServiceState{}, want: want{manager: "none"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			unitDir, home, dir := t.TempDir(), t.TempDir(), t.TempDir()
			if c.setup != nil {
				c.setup(t, unitDir, home)
			}
			var query func() (service.WindowsServiceState, error)
			if c.win != nil {
				st := *c.win
				query = func() (service.WindowsServiceState, error) { return st, nil }
			}
			sd := testStatusDeps(nil)
			sd.lifecycle = probeLifecycle(c.goos, startTarget(home), service.Deps{Runner: systemdAnswers(c.run), SystemUnitDir: unitDir}, query)
			if c.denied {
				sd.live = func(d string) agent.LiveSource {
					return localLive{stateDir: d, backend: replyBackend{err: fmt.Errorf("dial: %w", client.ErrPermissionDenied)}}
				}
			}
			m := statusJSON(t, sd, "--state-dir", dir)
			l, _ := m["lifecycle"].(map[string]any)
			if l == nil {
				t.Fatalf("lifecycle = %v", m["lifecycle"])
			}
			got := want{
				manager: l["manager"].(string), scope: l["scope"].(string), unit: l["unit"].(string), hint: l["start_hint"].(string),
				installed: l["installed"].(bool), running: l["running"].(bool),
				canStart: l["can_start"].(bool), canStop: l["can_stop"].(bool), otherAcc: m["other_account_worker"].(bool),
			}
			if c.want.manager == "none" {
				c.want.scope, c.want.unit = "", ""
			}
			if got != c.want {
				t.Fatalf("lifecycle = %+v, want %+v", got, c.want)
			}
			if v, ok := l["service_binary_trusted"]; !ok || v != nil {
				t.Fatalf("service_binary_trusted = %v (present %v), want null", v, ok)
			}
			if _, ok := l["service_binary"]; ok {
				t.Fatal("status names a service binary")
			}
			if raw := fmt.Sprint(m); strings.Contains(strings.ToLower(raw), "minion.exe") {
				t.Fatalf("status names a binary path: %s", raw)
			}
		})
	}
}

// A service manager that cannot be asked is a problem line, not a failure,
// and the block says nothing is known to be installed.
func TestLifecycleQueryErrorIsAProblem(t *testing.T) {
	sd := testStatusDeps(nil)
	sd.lifecycle = probeLifecycle("windows", startTarget(t.TempDir()), service.Deps{},
		func() (service.WindowsServiceState, error) {
			return service.WindowsServiceState{}, errors.New("access denied")
		})
	m := statusJSON(t, sd, "--state-dir", t.TempDir())
	if l := m["lifecycle"].(map[string]any); l["manager"] != "none" || l["installed"] != false {
		t.Fatalf("lifecycle = %v", l)
	}
	if !strings.Contains(fmt.Sprint(m["problems"]), "service manager") {
		t.Fatalf("problems = %v", m["problems"])
	}
}
