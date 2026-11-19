package cli

import (
	"testing"

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
