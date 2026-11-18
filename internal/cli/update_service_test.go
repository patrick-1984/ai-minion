package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

const (
	svcExe  = `C:\Program Files\Minion\minion.exe`
	userExe = `C:\Users\alice\AppData\Local\Programs\minion\minion.exe`
)

func TestServiceUpdateGuard(t *testing.T) {
	installed := service.WindowsServiceState{Installed: true, BinaryPath: svcExe}
	for name, c := range map[string]struct {
		st        service.WindowsServiceState
		exe       string
		elevated  bool
		refuse    []string
		note      []string
		quietNote bool
	}{
		"no service":          {st: service.WindowsServiceState{}, exe: userExe, quietNote: true},
		"unknown binary":      {st: service.WindowsServiceState{Installed: true}, exe: userExe, quietNote: true},
		"service copy, user":  {st: installed, exe: `c:\program files\minion\MINION.exe`, refuse: []string{"only administrators", "elevated prompt", `"` + svcExe + `" update`, "Hub"}},
		"service copy, admin": {st: installed, exe: svcExe, elevated: true, note: []string{"Restart-Service MinionWorker"}},
		"user copy":           {st: installed, exe: userExe, note: []string{"did not change", svcExe, "elevated prompt", "Restart-Service MinionWorker"}},
		"user copy, elevated": {st: installed, exe: userExe, elevated: true, note: []string{"did not change"}},
	} {
		refuse, note := serviceUpdateGuard(c.st, c.exe, c.elevated)
		if (refuse != "") != (len(c.refuse) > 0) {
			t.Errorf("%s: refuse = %q", name, refuse)
		}
		for _, w := range c.refuse {
			if !strings.Contains(refuse, w) {
				t.Errorf("%s: refusal lacks %q: %s", name, w, refuse)
			}
		}
		if c.quietNote && note != "" {
			t.Errorf("%s: note = %q", name, note)
		}
		for _, w := range c.note {
			if !strings.Contains(note, w) {
				t.Errorf("%s: note lacks %q: %s", name, w, note)
			}
		}
	}
}

func TestUpdateRefusesTheServiceBinaryWithExit21BeforeContactingAnything(t *testing.T) {
	h := newHarness(t)
	h.src.addRelease(t, "v1.4.2", "1.0", "1.1", false)
	var gotExe string
	h.guard = func(exe string) (string, string) { gotExe = exe; return "run it elevated", "" }
	out, err := h.run("v1.0.0")
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != 21 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "run it elevated") {
		t.Errorf("output = %q", out)
	}
	if gotExe != h.exec {
		t.Errorf("guard checked %q, want %q", gotExe, h.exec)
	}
	if h.updaterBuilt || h.installed() != "installed" {
		t.Errorf("the update went ahead: updater %v, installed %q", h.updaterBuilt, h.installed())
	}
}

func TestUpdatePrintsTheServiceNoteAfterAnUpdate(t *testing.T) {
	h := newHarness(t)
	h.src.addRelease(t, "v1.4.2", "1.0", "1.1", false)
	h.guard = func(string) (string, string) { return "", "the service runs its own copy" }
	out, err := h.run("v1.0.0")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if h.installed() != "binary v1.4.2" || !strings.Contains(out, "the service runs its own copy") {
		t.Errorf("installed %q, output %q", h.installed(), out)
	}
}

func TestWorkerStatusWarnsAboutAWritableServiceBinary(t *testing.T) {
	dir := t.TempDir()
	sd := testStatusDeps(nil)
	sd.serviceBinary = func() string { return "the service binary X is writable by non-administrators" }
	out, _, err := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--json", "--state-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"problems"`) || !strings.Contains(out, "writable by non-administrators") {
		t.Errorf("status lacks the warning:\n%s", out)
	}
	sd.serviceBinary = func() string { return "" }
	if out, _, err = runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--json", "--state-dir", dir); err != nil || strings.Contains(out, `"problems"`) {
		t.Errorf("status of a safe service: %v\n%s", err, out)
	}
}
