package cli

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

type startRig struct {
	d        workerStartDeps
	ran      [][]string
	loaded   map[string]string // systemctl show output by unit
	launched [][]string
	liveNow  bool
	supRuns  bool
	// templateOnly: the unit is loaded from the packaged template, not enabled.
	templateOnly bool
}

func newStartRig(goos string) *startRig {
	r := &startRig{loaded: map[string]string{}}
	r.d = workerStartDeps{
		goos: goos,
		run: func(_ context.Context, name string, args ...string) error {
			r.ran = append(r.ran, append([]string{name}, args...))
			// A started Worker answers from now on.
			if len(args) > 0 && (args[len(args)-2] == "start" || args[0] == "kickstart") {
				r.liveNow = true
			}
			return nil
		},
		output: func(_ context.Context, _ string, args ...string) (string, error) {
			unit := args[len(args)-1]
			switch {
			case args[len(args)-3] == "ActiveState":
				return r.loaded["active:"+unit], nil
			case args[len(args)-3] == "UnitFileState":
				if r.loaded[unit] == "loaded" && !r.templateOnly {
					return "enabled", nil
				}
				return "disabled", nil
			}
			return r.loaded[unit], nil
		},
		autostart: func() (string, bool, error) {
			return `"C:\Program Files\minion.exe" worker launch --state-dir "D:\my state"`, true, nil
		},
		supervisorRunning: func() bool { return r.supRuns },
		startLauncher: func(exe string, args []string) error {
			r.launched = append(r.launched, append([]string{exe}, args...))
			r.liveNow = true
			return nil
		},
		call: func(context.Context, string) error {
			if r.liveNow {
				return nil
			}
			return errors.New("not running")
		},
		fileExists: func(string) bool { return true },
		home:       func() string { return "/home/u" },
		uid:        func() int { return 501 },
		euid:       func() int { return 1000 },
		user:       func() string { return "u" },
		elevation:  func() service.Elevation { return service.Elevation{} },
		isSystem:   func() bool { return false },
		sleep:      func(time.Duration) {},
		polls:      2,
	}
	return r
}

func (r *startRig) start(t *testing.T) (string, error) {
	t.Helper()
	cmd := newWorkerStartCmd(r.d)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	cmd.SetContext(context.Background())
	err := runWorkerStart(cmd, r.d)
	return out.String(), err
}

func TestStartWindowsStartsTheRegisteredSupervisor(t *testing.T) {
	r := newStartRig("windows")
	out, err := r.start(t)
	if err != nil || !strings.Contains(out, "started") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	want := [][]string{{`C:\Program Files\minion.exe`, "--state-dir", `D:\my state`}}
	if !reflect.DeepEqual(r.launched, want) {
		t.Fatalf("launched %q, want %q", r.launched, want)
	}
}

func TestStartLeavesARunningWorkerAlone(t *testing.T) {
	for _, goos := range []string{"windows", "linux", "darwin"} {
		r := newStartRig(goos)
		r.liveNow = true
		out, err := r.start(t)
		if err != nil || !strings.Contains(out, "already running") || len(r.ran)+len(r.launched) != 0 {
			t.Errorf("%s: out=%q err=%v ran=%v launched=%v", goos, out, err, r.ran, r.launched)
		}
	}
	// A supervisor between two Worker runs counts as running (Windows).
	r := newStartRig("windows")
	r.supRuns = true
	if out, err := r.start(t); err != nil || !strings.Contains(out, "already running") || len(r.launched) != 0 {
		t.Errorf("supervisor: out=%q err=%v launched=%v", out, err, r.launched)
	}
	// A loaded unit that is active is left alone (the socket not yet up).
	r = newStartRig("linux")
	r.loaded["minion-worker.service"] = "loaded"
	r.loaded["active:minion-worker.service"] = "activating"
	if out, err := r.start(t); err != nil || !strings.Contains(out, "already running") || len(r.ran) != 0 {
		t.Errorf("unit: out=%q err=%v ran=%v", out, err, r.ran)
	}
}

func TestStartNotInstalledPointsToUp(t *testing.T) {
	r := newStartRig("windows")
	r.d.autostart = func() (string, bool, error) { return "", false, nil }
	if _, err := r.start(t); err == nil || !strings.Contains(err.Error(), "minion worker up") {
		t.Errorf("windows: %v", err)
	}
	r = newStartRig("linux")
	r.loaded["minion-worker.service"] = "not-found"
	r.loaded["minion-worker@u.service"] = "not-found"
	if _, err := r.start(t); err == nil || !strings.Contains(err.Error(), "minion worker up") || len(r.ran) != 0 {
		t.Errorf("linux: %v %v", err, r.ran)
	}
	r = newStartRig("darwin")
	r.d.fileExists = func(string) bool { return false }
	if _, err := r.start(t); err == nil || !strings.Contains(err.Error(), "minion worker up") {
		t.Errorf("darwin: %v", err)
	}
}

func TestStartLinuxUserAndSystemUnit(t *testing.T) {
	r := newStartRig("linux")
	r.loaded["minion-worker.service"] = "loaded"
	if out, err := r.start(t); err != nil || !strings.Contains(out, "started") {
		t.Fatalf("user unit: %q %v", out, err)
	}
	if want := []string{"systemctl", "--user", "start", "minion-worker.service"}; !reflect.DeepEqual(r.ran[len(r.ran)-1], want) {
		t.Errorf("ran %v", r.ran)
	}
	r = newStartRig("linux")
	r.loaded["minion-worker@u.service"] = "loaded"
	if _, err := r.start(t); err == nil || !strings.Contains(err.Error(), "sudo systemctl start minion-worker@u.service") {
		t.Errorf("system unit as a normal user: %v", err)
	}
}

func TestStartMacLoadsAndKickstartsWithoutRestart(t *testing.T) {
	r := newStartRig("darwin")
	if out, err := r.start(t); err != nil || !strings.Contains(out, "started") {
		t.Fatalf("%q %v", out, err)
	}
	last := r.ran[len(r.ran)-1]
	if last[1] != "kickstart" || last[2] != "gui/501/io.github.patrick-1984.minion-worker" {
		t.Errorf("last command %v (kickstart -k would restart a running agent)", last)
	}
}

func TestStartWindowsRefusesAnElevatedPromptAndSystem(t *testing.T) {
	r := newStartRig("windows")
	r.d.elevation = func() service.Elevation { return service.Elevation{Admin: true, Elevated: true} }
	if _, err := r.start(t); err == nil || !strings.Contains(err.Error(), "administrator") || len(r.launched) != 0 {
		t.Fatalf("elevated: err=%v launched=%v", err, r.launched)
	}
	r = newStartRig("windows")
	r.d.isSystem = func() bool { return true }
	if _, err := r.start(t); !errors.Is(err, service.ErrRunsAsSystem) || len(r.launched) != 0 {
		t.Fatalf("system: err=%v launched=%v", err, r.launched)
	}
	// An administrator in a normal (split token, not elevated) prompt may start it.
	r = newStartRig("windows")
	r.d.elevation = func() service.Elevation { return service.Elevation{Admin: true} }
	if _, err := r.start(t); err != nil || len(r.launched) != 1 {
		t.Fatalf("non-elevated admin: err=%v launched=%v", err, r.launched)
	}
}

// The packaged template alone is not a configured Worker: no sudo hint, no start.
func TestStartLinuxTemplateOnlyIsNotInstalled(t *testing.T) {
	r := newStartRig("linux")
	r.loaded["minion-worker@u.service"] = "loaded"
	r.templateOnly = true
	if _, err := r.start(t); err == nil || !strings.Contains(err.Error(), "minion worker up") || strings.Contains(err.Error(), "sudo") || len(r.ran) != 0 {
		t.Fatalf("err=%v ran=%v", err, r.ran)
	}
}

func TestSplitWindowsCommand(t *testing.T) {
	got := splitWindowsCommand(`"C:\a b\m.exe" worker launch --state-dir "D:\x y" "q\"r"`)
	want := []string{`C:\a b\m.exe`, "worker", "launch", "--state-dir", `D:\x y`, `q"r`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
}
