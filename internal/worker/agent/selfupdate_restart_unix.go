//go:build !windows

package agent

import (
	"log/slog"
	"os"
	"syscall"
)

// underServiceManager reports whether systemd or launchd runs this process:
// they start it again after it exits (Restart=always, KeepAlive).
func underServiceManager() bool {
	if os.Getenv("INVOCATION_ID") != "" {
		return true
	}
	x := os.Getenv("XPC_SERVICE_NAME")
	return x != "" && x != "0"
}

// restartProcess starts the new binary: a service exits with exitRestart and
// its manager starts it again; in the foreground the process replaces itself
// (same PID, same terminal), as Syncthing does. A failed exec exits non-zero
// rather than running on without its modules.
func restartProcess(log *slog.Logger, exe string) {
	if underServiceManager() {
		os.Exit(exitRestart)
	}
	err := syscall.Exec(exe, os.Args, os.Environ())
	log.Error("cannot restart the Worker with the new binary; start it again by hand", "error", err.Error())
	os.Exit(1)
}

// restartNow is restartProcess: Unix replaces the process in place either way.
func restartNow(log *slog.Logger, exe string) { restartProcess(log, exe) }

// waitForParent is only needed on Windows: elsewhere the process replaces
// itself in place (syscall.Exec).
func waitForParent() {}
