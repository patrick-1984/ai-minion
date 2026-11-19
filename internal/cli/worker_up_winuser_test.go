package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// fakeWin is a Windows machine for the Windows path of "worker up".
type fakeWin struct {
	legacy   service.WindowsServiceState
	elev     service.Elevation
	removed  bool
	runValue string
	running  bool
	starts   []string
	stops    int
	noStart  bool // the started supervisor never brings a Worker up
	events   []string
}

func (f *fakeWin) deps() *windowsUpDeps {
	return &windowsUpDeps{
		query:     func() (service.WindowsServiceState, error) { return f.legacy, nil },
		elevation: func() service.Elevation { return f.elev },
		removeLegacy: func(context.Context) error {
			f.events = append(f.events, "remove")
			f.removed, f.legacy = true, service.WindowsServiceState{}
			return nil
		},
		setAutostart: func(cmd string) error { f.events = append(f.events, "run-entry"); f.runValue = cmd; return nil },
		call: func(_ context.Context, op string) (json.RawMessage, error) {
			if !f.running {
				return nil, client.ErrWorkerNotRunning
			}
			if op == agent.OpLocalStop {
				f.events = append(f.events, "stop")
				f.stops++
				f.running = false
			}
			return json.RawMessage(`{}`), nil
		},
		startLauncher: func(exe string, args []string) error {
			f.events = append(f.events, "launch")
			f.starts = append(f.starts, exe+" "+strings.Join(args, " "))
			if !f.noStart {
				f.running = true
			}
			return nil
		},
		sleep: func(time.Duration) {},
		polls: 3,
	}
}

func newWinUp(t *testing.T) (*upFixture, *fakeWin) {
	t.Helper()
	f := newUpFixture(t, false)
	w := &fakeWin{}
	f.deps.detectEnv = service.Env{GOOS: "windows"}
	f.deps.executable = func() (string, error) { return `C:\Users\ann\AppData\Local\Programs\minion\minion.exe`, nil }
	f.deps.windows = w.deps()
	return f, w
}

// The Worker runs as the user: a Run entry starts its supervisor at logon,
// and it is started now. No service is installed.
func TestWorkerUpWindowsInstallsTheLogonAutostart(t *testing.T) {
	f, w := newWinUp(t)
	out, err := f.run("--share-existing", "none")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// The Run entry always names the state directory chosen now.
	if want := `C:\Users\ann\AppData\Local\Programs\minion\minion.exe worker launch --state-dir ` + f.stateDir(); w.runValue != want {
		t.Errorf("Run value = %q", w.runValue)
	}
	if got := strings.Join(w.events, ","); got != "run-entry,launch" {
		t.Errorf("events = %s", got)
	}
	if !strings.Contains(out, "runs as you") || !strings.Contains(out, "logon") {
		t.Errorf("output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(f.stateDir(), "config.json")); err != nil {
		t.Errorf("settings not saved: %v", err)
	}
}

// A state directory that is not the default one goes into the Run entry.
func TestWorkerUpWindowsPinsTheStateDirectory(t *testing.T) {
	f, w := newWinUp(t)
	dir := filepath.Join(t.TempDir(), "my state")
	if _, err := f.run("--share-existing", "none", "--state-dir", dir); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(w.runValue, ` worker launch --state-dir "`+dir+`"`) || w.starts[0] != `C:\Users\ann\AppData\Local\Programs\minion\minion.exe --state-dir `+dir {
		t.Errorf("Run value %q, started %q", w.runValue, w.starts)
	}
}

// A Worker that runs already is stopped so that the new settings apply, then
// started again by the supervisor.
func TestWorkerUpWindowsReplacesARunningWorker(t *testing.T) {
	f, w := newWinUp(t)
	w.running = true
	if _, err := f.run("--share-existing", "none"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(w.events, ","); got != "stop,run-entry,launch" || !w.running {
		t.Errorf("events = %s, running %v", got, w.running)
	}
}

func TestWorkerUpWindowsReportsAWorkerThatDoesNotStart(t *testing.T) {
	f, w := newWinUp(t)
	w.noStart = true
	_, err := f.run("--share-existing", "none")
	if err == nil || !strings.Contains(err.Error(), "did not start") || !strings.Contains(err.Error(), launchLogName) {
		t.Fatalf("err = %v", err)
	}
}

// The LocalSystem service of an older release must go first. Without an
// administrator nothing is saved or changed, the exit status is 21 and the
// message names only Windows tools.
func TestWorkerUpWindowsRefusesWhileTheLegacyServiceExists(t *testing.T) {
	f, w := newWinUp(t)
	w.legacy = service.WindowsServiceState{Installed: true, Running: true, Account: "LocalSystem"}
	_, err := f.run("--share-existing", "none")
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != 21 {
		t.Fatalf("err = %v", err)
	}
	msg := f.stderr.String()
	if !strings.Contains(msg, "sc.exe delete MinionWorker") || strings.Contains(msg, "minion.exe") {
		t.Errorf("message = %s", msg)
	}
	if len(w.events) != 0 {
		t.Errorf("events = %v", w.events)
	}
	if _, err := os.Stat(filepath.Join(f.stateDir(), "config.json")); err == nil {
		t.Error("settings were saved")
	}
}

// An administrator's worker up removes the old service. Without a UAC split
// (an account that is always administrator) it then installs the Worker.
func TestWorkerUpWindowsRemovesTheLegacyServiceAsAdministrator(t *testing.T) {
	f, w := newWinUp(t)
	w.legacy = service.WindowsServiceState{Installed: true, Account: "LocalSystem"}
	w.elev = service.Elevation{Admin: true}
	out, err := f.run("--share-existing", "none")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := strings.Join(w.events, ","); got != "remove,run-entry,launch" || !strings.Contains(out, "Removed the service MinionWorker") {
		t.Errorf("events = %s, output:\n%s", got, out)
	}
}

// From an elevated prompt (the elevated half of a UAC split token) the old
// service is removed, but the Worker is not installed: it would run elevated.
func TestWorkerUpWindowsRefusesAnElevatedPrompt(t *testing.T) {
	f, w := newWinUp(t)
	w.legacy = service.WindowsServiceState{Installed: true, Account: "LocalSystem"}
	w.elev = service.Elevation{Admin: true, Elevated: true}
	_, err := f.run("--share-existing", "none")
	if err == nil || !strings.Contains(err.Error(), "normal terminal") {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(w.events, ","); got != "remove" {
		t.Errorf("events = %s", got)
	}
}

// "worker service-run" (the entry point of the old service) no longer runs
// the Worker: it says why and ends cleanly, so the service is not restarted.
func TestWorkerServiceRunEndsAnOldServiceCleanly(t *testing.T) {
	var stderr bytes.Buffer
	cmd := newWorkerServiceRunCmd(workerRunDeps{stderr: &stderr}, service.SCM{GOOS: "linux"})
	cmd.SetContext(context.Background())
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "no longer runs the Worker as the LocalSystem service") ||
		!strings.Contains(stderr.String(), "sc.exe delete MinionWorker") {
		t.Errorf("stderr = %s", stderr.String())
	}
}

func TestWorkerStatusWarnsAboutTheLegacyService(t *testing.T) {
	dir := t.TempDir()
	sd := testStatusDeps(nil)
	sd.windowsService = func() (service.WindowsServiceState, error) {
		return service.WindowsServiceState{Installed: true, Running: true, Account: "LocalSystem"}, nil
	}
	out, _, err := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--json", "--state-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"problems"`) || !strings.Contains(out, "LocalSystem") || !strings.Contains(out, "sc.exe delete MinionWorker") {
		t.Errorf("status:\n%s", out)
	}
}

// LocalSystem (a deployment task) has no UAC split token, so the elevation
// check misses it: the token SID is checked, before anything is changed.
func TestWorkerUpWindowsRefusesLocalSystem(t *testing.T) {
	f, w := newWinUp(t)
	d := w.deps()
	d.isSystem = func() bool { return true }
	f.deps.windows = d
	_, err := f.run("--share-existing", "none")
	if !errors.Is(err, service.ErrRunsAsSystem) {
		t.Fatalf("err = %v", err)
	}
	if len(w.events) != 0 {
		t.Errorf("events = %v", w.events)
	}
}

// A service manager that cannot be asked is not "no service": fail closed
// with the trusted removal command, and change nothing.
func TestWorkerUpWindowsFailsClosedWhenTheServiceQueryFails(t *testing.T) {
	f, w := newWinUp(t)
	d := w.deps()
	d.query = func() (service.WindowsServiceState, error) {
		return service.WindowsServiceState{}, errors.New("Access is denied")
	}
	f.deps.windows = d
	_, err := f.run("--share-existing", "none")
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != 21 {
		t.Fatalf("err = %v", err)
	}
	if msg := f.stderr.String(); !strings.Contains(msg, "Access is denied") || !strings.Contains(msg, "sc.exe delete MinionWorker") {
		t.Errorf("message = %s", msg)
	}
	if len(w.events) != 0 {
		t.Errorf("events = %v", w.events)
	}
}

// A removal that only marked the service for deletion is not reported as
// "Removed"; one that leaves the service running is an error.
func TestWorkerUpWindowsDistinguishesPendingAndFailedRemoval(t *testing.T) {
	f, w := newWinUp(t)
	w.legacy = service.WindowsServiceState{Installed: true, Account: "LocalSystem"}
	w.elev = service.Elevation{Admin: true}
	d := w.deps()
	d.removeLegacy = func(context.Context) error { return service.ErrLegacyRemovalPending }
	f.deps.windows = d
	out, err := f.run("--share-existing", "none")
	if err != nil || strings.Contains(out, "Removed the service") || !strings.Contains(out, "marked for deletion") {
		t.Fatalf("err=%v out:\n%s", err, out)
	}

	f, w = newWinUp(t)
	w.legacy = service.WindowsServiceState{Installed: true, Running: true, Account: "LocalSystem"}
	w.elev = service.Elevation{Admin: true}
	d = w.deps()
	d.removeLegacy = func(context.Context) error { return nil } // the fake leaves it running
	f.deps.windows = d
	out, err = f.run("--share-existing", "none")
	if err == nil || strings.Contains(out, "Removed the service") || len(w.events) != 0 {
		t.Fatalf("err=%v events=%v out:\n%s", err, w.events, out)
	}
}

// The replacement starts only after the old supervisor released its mutex,
// or the new one would find it taken and end, leaving no Worker.
func TestWorkerUpWindowsWaitsForTheOldSupervisor(t *testing.T) {
	f, w := newWinUp(t)
	w.running = true
	d := w.deps()
	checks := 0
	d.supervisorRunning = func() bool {
		checks++
		if checks < 3 {
			return true
		}
		w.events = append(w.events, "old-supervisor-gone")
		return false
	}
	f.deps.windows = d
	if _, err := f.run("--share-existing", "none"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(w.events, ","); got != "stop,old-supervisor-gone,run-entry,launch" {
		t.Errorf("events = %s", got)
	}

	f, w = newWinUp(t)
	w.running = true
	d = w.deps()
	d.supervisorRunning = func() bool { return true }
	f.deps.windows = d
	_, err := f.run("--share-existing", "none")
	if err == nil || !strings.Contains(err.Error(), "supervisor did not end") || len(w.starts) != 0 {
		t.Fatalf("err=%v starts=%v", err, w.starts)
	}
}

// The Worker that answers must serve the requested state directory.
func TestWorkerUpWindowsChecksTheStateDirectoryOfTheAnsweringWorker(t *testing.T) {
	f, w := newWinUp(t)
	d := w.deps()
	inner := d.call
	d.call = func(ctx context.Context, op string) (json.RawMessage, error) {
		raw, err := inner(ctx, op)
		if err == nil && op == agent.OpLocalStatus {
			return json.RawMessage(`{"state_dir":"C:\\elsewhere"}`), nil
		}
		return raw, err
	}
	f.deps.windows = d
	_, err := f.run("--share-existing", "none")
	if err == nil || !strings.Contains(err.Error(), "C:\\elsewhere") {
		t.Fatalf("err = %v", err)
	}
}

// The old service's entry point reports through the manager handshake
// without sending a stop to itself.
func TestWorkerServiceRunUnderTheManagerEndsWithoutStopControl(t *testing.T) {
	var stderr bytes.Buffer
	ended := false
	scm := service.SCM{GOOS: "windows", Interactive: func() bool { return false },
		EndUnderSCM: func(fn func()) error { fn(); ended = true; return nil }}
	cmd := newWorkerServiceRunCmd(workerRunDeps{stderr: &stderr}, scm)
	cmd.SetContext(context.Background())
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil || !ended || !strings.Contains(stderr.String(), "no longer runs") {
		t.Fatalf("err=%v ended=%v stderr=%s", err, ended, stderr.String())
	}
}

func TestWorkerStatusReportsAServiceQueryFailure(t *testing.T) {
	dir := t.TempDir()
	sd := testStatusDeps(nil)
	sd.windowsService = func() (service.WindowsServiceState, error) {
		return service.WindowsServiceState{}, errors.New("Access is denied")
	}
	out, _, err := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--json", "--state-dir", dir)
	if err != nil || !strings.Contains(out, "Access is denied") || !strings.Contains(out, "sc.exe delete MinionWorker") {
		t.Errorf("err=%v status:\n%s", err, out)
	}
}

// Round 2: an explicit default --state-dir against a conflicting
// MINION_STATE_DIR: the Run entry and the immediate start both name it.
func TestWorkerUpWindowsPinsTheExplicitDefaultAgainstTheEnvironment(t *testing.T) {
	f, w := newWinUp(t)
	f.deps.run.lookupEnv = func(k string) (string, bool) {
		if k == "MINION_STATE_DIR" {
			return filepath.Join(t.TempDir(), "other-profile"), true
		}
		return "", false
	}
	if _, err := f.run("--share-existing", "none", "--state-dir", f.stateDir()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(w.runValue, " --state-dir "+f.stateDir()) || !strings.HasSuffix(w.starts[0], " --state-dir "+f.stateDir()) {
		t.Errorf("Run value %q, started %q", w.runValue, w.starts)
	}
}

// A supervisor in its back-off answers on no pipe: it is signaled directly,
// before the Run value changes, and a failed replacement leaves the old Run
// entry as it was.
func TestWorkerUpWindowsSignalsABackingOffSupervisor(t *testing.T) {
	f, w := newWinUp(t)
	d := w.deps() // no Worker answers (running is false)
	d.signalSupervisor = func() (bool, error) { w.events = append(w.events, "signal"); return true, nil }
	f.deps.windows = d
	if _, err := f.run("--share-existing", "none"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(w.events, ","); got != "signal,run-entry,launch" {
		t.Errorf("events = %s", got)
	}

	f, w = newWinUp(t)
	d = w.deps()
	d.supervisorRunning = func() bool { return true } // never ends
	f.deps.windows = d
	if _, err := f.run("--share-existing", "none"); err == nil || w.runValue != "" {
		t.Fatalf("err=%v runValue=%q: a failed replacement must not write the Run entry", err, w.runValue)
	}
}

// LocalSystem is refused before the foreground setup (the psmux offer, the
// saved settings) and before a join token is expanded.
func TestWorkerUpForegroundRefusesLocalSystemFirst(t *testing.T) {
	old := systemAccount
	systemAccount = func() bool { return true }
	defer func() { systemAccount = old }()
	f, w := newWinUp(t)
	_, err := f.run("--foreground", "--share-existing", "none")
	if !errors.Is(err, service.ErrRunsAsSystem) || len(w.events) != 0 {
		t.Fatalf("err=%v events=%v", err, w.events)
	}
	if _, err := os.Stat(filepath.Join(f.stateDir(), "config.json")); err == nil {
		t.Error("settings were saved before the refusal")
	}
}

// Round 3: only the supervisor stops its Worker; a connection that ends as
// the Worker shuts down is not an error.
func TestWorkerUpWindowsLetsTheSupervisorStopItsWorker(t *testing.T) {
	f, w := newWinUp(t)
	w.running = true
	d := w.deps()
	d.signalSupervisor = func() (bool, error) {
		w.events = append(w.events, "signal")
		w.running = false // the supervisor stopped its Worker
		return true, nil
	}
	d.supervisorRunning = func() bool { return w.running }
	f.deps.windows = d
	if _, err := f.run("--share-existing", "none"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(w.events, ","); got != "signal,run-entry,launch" {
		t.Errorf("events = %s (no second stop request over the pipe)", got)
	}

	f, w = newWinUp(t)
	w.running = true
	d = w.deps()
	inner := d.call
	d.call = func(ctx context.Context, op string) (json.RawMessage, error) {
		if op == agent.OpLocalStop {
			w.running = false
			return nil, client.ErrConnectionLost // the Worker closed the pipe
		}
		return inner(ctx, op)
	}
	f.deps.windows = d
	if _, err := f.run("--share-existing", "none"); err != nil {
		t.Fatalf("a connection lost during the stop must count as stopped: %v", err)
	}
}

// A Run key that cannot be written is found before the Worker is stopped.
func TestWorkerUpWindowsChecksTheRunKeyBeforeStopping(t *testing.T) {
	f, w := newWinUp(t)
	w.running = true
	d := w.deps()
	d.checkWritable = func() error { return errors.New("Access is denied") }
	f.deps.windows = d
	_, err := f.run("--share-existing", "none")
	if err == nil || !strings.Contains(err.Error(), "Access is denied") || !strings.Contains(err.Error(), "not touched") {
		t.Fatalf("err = %v", err)
	}
	if !w.running || w.stops != 0 || len(w.events) != 0 {
		t.Errorf("the Worker was touched: running=%v events=%v", w.running, w.events)
	}
}

// LocalSystem is refused before the hub line reads any state.
func TestWithHubLineSkipsTheStateReadsForLocalSystem(t *testing.T) {
	old := systemAccount
	systemAccount = func() bool { return true }
	defer func() { systemAccount = old }()
	read := false
	ran := false
	run := withHubLine(func(*cobra.Command, []string) error { ran = true; return nil },
		func(string) (string, bool) { read = true; return "", false })
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := run(cmd, nil); err != nil || !ran || read {
		t.Fatalf("err=%v ran=%v read=%v", err, ran, read)
	}
}
