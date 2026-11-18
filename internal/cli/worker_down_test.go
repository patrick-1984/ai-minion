package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

func downCmd(t *testing.T, d workerDownDeps) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	err := runWorkerDown(cmd, d)
	return out.String(), err
}

func baseDown() workerDownDeps {
	return workerDownDeps{goos: "linux",
		run:            func(context.Context, string, ...string) error { return errors.New("inactive") },
		output:         func(context.Context, string, ...string) (string, error) { return "inactive\n", nil },
		windowsService: func() (bool, error) { return false, nil },
		euid:           func() int { return 1000 },
		user:           func() string { return "patryk" },
		sleep:          func(time.Duration) {},
		stateFree:      func(string) bool { return true },
	}
}

func TestWorkerDownStopsAForegroundWorker(t *testing.T) {
	d := baseDown()
	stopped := false
	d.call = func(_ context.Context, op string) (json.RawMessage, error) {
		if op == agent.OpLocalStop {
			stopped = true
			return nil, nil
		}
		if stopped {
			return nil, client.ErrWorkerNotRunning
		}
		return json.RawMessage(`{"state_dir":"/custom"}`), nil
	}
	// The lock is still held for a moment after the socket closed; the one
	// checked is the running Worker's own state directory.
	free := 0
	d.stateFree = func(dir string) bool {
		if dir != "/custom" {
			t.Errorf("checked %q", dir)
		}
		free++
		return free > 3
	}
	out, err := downCmd(t, d)
	if err != nil || !stopped || free < 4 || !strings.Contains(out, "minion worker up --foreground") {
		t.Fatalf("out=%q err=%v stopped=%v free=%d", out, err, stopped, free)
	}
}

func TestWorkerDownWithNothingRunning(t *testing.T) {
	d := baseDown()
	d.call = func(context.Context, string) (json.RawMessage, error) { return nil, client.ErrWorkerNotRunning }
	out, err := downCmd(t, d)
	if err != nil || !strings.Contains(out, "No Worker is running") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestWorkerDownPrintsTheSudoCommandForASystemService(t *testing.T) {
	d := baseDown()
	// "activating": the wait before an automatic restart counts as running.
	d.output = func(_ context.Context, name string, args ...string) (string, error) {
		if args[len(args)-1] == "minion-worker@patryk.service" && args[0] == "show" {
			return "activating\n", nil
		}
		return "inactive\n", nil
	}
	d.call = func(_ context.Context, op string) (json.RawMessage, error) {
		if op == agent.OpLocalStatus {
			return nil, client.ErrWorkerNotRunning // the service's Worker does not answer here
		}
		t.Fatal("the socket was used to stop a service")
		return nil, nil
	}
	_, err := downCmd(t, d)
	if err == nil || !strings.Contains(err.Error(), "sudo systemctl stop minion-worker@patryk.service") {
		t.Fatalf("err = %v", err)
	}
}

func TestWorkerDownStopsTheUserServiceAndTheWindowsService(t *testing.T) {
	d := baseDown()
	var ran []string
	d.output = func(_ context.Context, name string, args ...string) (string, error) {
		if args[0] == "--user" {
			return "active\n", nil
		}
		return "inactive\n", nil
	}
	d.run = func(_ context.Context, name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil
	}
	out, err := downCmd(t, d)
	if err != nil || !strings.Contains(strings.Join(ran, ";"), "systemctl --user stop minion-worker.service") || !strings.Contains(out, "systemctl --user start") {
		t.Fatalf("ran=%v out=%q err=%v", ran, out, err)
	}
	w := baseDown()
	w.goos = "windows"
	w.windowsService = func() (bool, error) { return true, nil }
	out, err = downCmd(t, w)
	if err != nil || !strings.Contains(out, "Start-Service MinionWorker") {
		t.Fatalf("windows: out=%q err=%v", out, err)
	}
	w.windowsService = func() (bool, error) { return true, errors.New("access denied") }
	if _, err := downCmd(t, w); err == nil || !strings.Contains(err.Error(), "Run as administrator") {
		t.Fatalf("windows denied: %v", err)
	}
}
