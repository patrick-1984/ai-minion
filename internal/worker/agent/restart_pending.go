package agent

import (
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
)

// waitPIDEnv names the process a restarted Worker waits for (Windows
// foreground, restartDetached).
const waitPIDEnv = "MINION_UPDATE_WAIT_PID"

// consoleChildEnv is environ without waitPIDEnv: a Worker that this process
// starts and waits for must never wait for this process in turn.
func consoleChildEnv(environ []string) []string {
	return slices.DeleteFunc(slices.Clone(environ), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return strings.EqualFold(k, waitPIDEnv)
	})
}

// A restart that must keep the console (Windows foreground, v0.9.3): instead
// of starting the new binary and exiting while it runs on in the background,
// the Worker records the binary, stops like on a signal (releasing the state
// lock and the control pipe), and the command that ran it starts the new
// binary in the same console and waits for it (RestartPending).
var (
	pendingRestart atomic.Pointer[string]
	currentStop    atomic.Pointer[func()]
)

// RestartPending returns the binary that the stopped Worker asked to be
// restarted with, or "".
func RestartPending() string {
	if p := pendingRestart.Load(); p != nil {
		return *p
	}
	return ""
}

// requestRestart records exe and asks the running runtime to stop. It reports
// false when no runtime runs (the caller then restarts on its own).
func requestRestart(exe string) bool {
	f := currentStop.Load()
	if f == nil {
		return false
	}
	pendingRestart.Store(&exe)
	(*f)()
	return true
}

// RestartDetached starts exe and exits, the way a restart worked before
// v0.9.3: the new process waits for this one to exit before it takes the
// state. The command uses it when the stop before a pending restart failed,
// so the exclusive resources are released only by this process's exit.
func RestartDetached(log *slog.Logger, exe string) { restartDetached(log, exe) }
