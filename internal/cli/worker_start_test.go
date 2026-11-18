package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/user"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

type startController struct{ calls []string }

func (c *startController) Install() error   { c.calls = append(c.calls, "install"); return nil }
func (c *startController) Uninstall() error { c.calls = append(c.calls, "uninstall"); return nil }
func (c *startController) Start() error     { c.calls = append(c.calls, "start"); return nil }
func (c *startController) Status() (service.Status, error) {
	c.calls = append(c.calls, "status")
	return service.StatusStopped, nil
}

type startRunner struct{ answer func(string) (string, error) }

func (r startRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	line := name
	for _, a := range args {
		line += " " + a
	}
	return r.answer(line)
}

func runStart(t *testing.T, d workerStartDeps) (map[string]any, int) {
	t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerStartCmd(d))
	var out, errOut bytes.Buffer
	code := execute(context.Background(), root, []string{"worker", "start", "--json"}, &out, &errOut)
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("%q: %v", out.String(), err)
	}
	return m, code
}

func startTarget(home string) service.TargetEnv {
	u := &user.User{Username: "ann", Uid: "1000", Gid: "1000", HomeDir: home}
	return service.TargetEnv{EUID: 1000, Getenv: func(string) string { return "" },
		Lookup: func(string) (*user.User, error) { return u, nil }, Current: func() (*user.User, error) { return u, nil }}
}

func TestWorkerStartNeverInstallsCLI(t *testing.T) {
	c := &startController{}
	m, code := runStart(t, workerStartDeps{goos: "windows", windows: func() (service.Controller, error) { return c, nil }})
	if code != ExitOK || m["manager"] != "windows" {
		t.Fatalf("start = %v, %d", m, code)
	}
	if len(c.calls) != 2 || c.calls[0] != "status" || c.calls[1] != "start" {
		t.Fatalf("calls = %v", c.calls)
	}
}

func TestWorkerStartExitCodes(t *testing.T) {
	// Not installed: 26.
	notInst := startRunner{answer: func(line string) (string, error) { return "inactive", errors.New("exit 3") }}
	m, code := runStart(t, workerStartDeps{goos: "linux", targetEnv: startTarget(t.TempDir()), service: service.Deps{Runner: notInst, SystemUnitDir: t.TempDir()}})
	if code != ExitNotInstalled || m["code"] != "not_installed" {
		t.Fatalf("not installed = %v, %d", m, code)
	}
	// System instance as non-root: 21 with the hint.
	sys := startRunner{answer: func(line string) (string, error) {
		if line == "systemctl is-enabled minion-worker@ann.service" {
			return "enabled", nil
		}
		return "inactive", errors.New("exit 3")
	}}
	m, code = runStart(t, workerStartDeps{goos: "linux", targetEnv: startTarget(t.TempDir()), service: service.Deps{Runner: sys, SystemUnitDir: t.TempDir()}})
	if code != ExitNeedsAdmin || m["start_hint"] != "sudo systemctl start minion-worker@ann.service" {
		t.Fatalf("system instance = %v, %d", m, code)
	}
}

func TestWorkerStopIsAnAliasOfDown(t *testing.T) {
	root := NewRoot()
	cmd, _, err := root.Find([]string{"worker", "stop"})
	if err != nil || cmd.Name() != "down" {
		t.Fatalf("worker stop = %v, %v", cmd, err)
	}
	if c, _, err := root.Find([]string{"worker", "start"}); err != nil || c.Name() != "start" {
		t.Fatalf("worker start = %v", err)
	}
}
