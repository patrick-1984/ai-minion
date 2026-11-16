package agent

import "sync/atomic"

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
