package cli

import (
	"bytes"
	"context"
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
		windowsService: func() (bool, error) { return false, nil },
		euid:           func() int { return 1000 },
		user:           func() string { return "patryk" },
		sleep:          func(time.Duration) {},
	}
}

func TestWorkerDownStopsAForegroundWorker(t *testing.T) {
	d := baseDown()
	stopped := false
	d.call = func(_ context.Context, op string) error {
		if op == agent.OpLocalStop {
			stopped = true
			return nil
		}
		if stopped {
			return client.ErrWorkerNotRunning
		}
		return nil
	}
	out, err := downCmd(t, d)
	if err != nil || !stopped || !strings.Contains(out, "minion worker up --foreground") {
		t.Fatalf("out=%q err=%v stopped=%v", out, err, stopped)
	}
}

func TestWorkerDownWithNothingRunning(t *testing.T) {
	d := baseDown()
	d.call = func(context.Context, string) error { return client.ErrWorkerNotRunning }
	out, err := downCmd(t, d)
	if err != nil || !strings.Contains(out, "No Worker is running") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestWorkerDownPrintsTheSudoCommandForASystemService(t *testing.T) {
	d := baseDown()
	d.run = func(_ context.Context, name string, args ...string) error {
		if name == "systemctl" && len(args) > 0 && args[0] == "is-active" && args[len(args)-1] == "minion-worker@patryk.service" {
			return nil
		}
		return errors.New("inactive")
	}
	d.call = func(context.Context, string) error { t.Fatal("the socket was used for a service"); return nil }
	_, err := downCmd(t, d)
	if err == nil || !strings.Contains(err.Error(), "sudo systemctl stop minion-worker@patryk.service") {
		t.Fatalf("err = %v", err)
	}
}

func TestWorkerDownStopsTheUserServiceAndTheWindowsService(t *testing.T) {
	d := baseDown()
	var ran []string
	d.run = func(_ context.Context, name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		if strings.Contains(strings.Join(args, " "), "--user") {
			return nil
		}
		return errors.New("inactive")
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
