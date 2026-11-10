package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// The Windows service runs the arguments of service.KardianosConfig; they must
// reach the SCM-aware entry point, not a command that never connects to the
// service control manager.
func TestWindowsServiceArgumentsReachTheSCMEntryPoint(t *testing.T) {
	cfg := service.KardianosConfig(`C:\Program Files\minion\minion.exe`)
	cmd, rest, err := NewRoot().Find(cfg.Arguments)
	if err != nil || cmd.Name() != service.WindowsServiceCommand || len(rest) != 0 {
		t.Fatalf("%v does not reach the service entry point: %v %v %v", cfg.Arguments, cmd, rest, err)
	}
	if !cmd.Hidden {
		t.Error("the service entry point must be hidden")
	}
	if cmd.Flags().Lookup("name") == nil {
		t.Error("the entry point does not take the Worker settings")
	}
}

func TestWorkerServiceRunRunsTheWorkerUntilStopped(t *testing.T) {
	deps, p := probeOnlyDeps(t, map[string]string{})
	root := workerRoot(deps)
	workerCommand(root).AddCommand(newWorkerServiceRunCmd(deps, service.SCM{GOOS: "linux"}))
	root.SetArgs([]string{"worker", service.WindowsServiceCommand})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	select {
	case <-p.ready:
	case err := <-done:
		t.Fatalf("ended before the Worker started: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the Worker did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("service-run = %v after the stop", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("service-run did not stop")
	}
	if got := strings.Join(p.log(), ","); got != "probe start,probe stop" {
		t.Errorf("events = %s", got)
	}
}
