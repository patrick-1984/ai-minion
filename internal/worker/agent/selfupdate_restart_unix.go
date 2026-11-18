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
		log.Info("restarting the Worker", "path", "service", "how", "exit with the restart code; the service manager starts the new binary", "exe", exe)
		os.Exit(exitRestart)
	}
	log.Info("restarting the Worker", "path", "exec", "how", "replace this process in place", "exe", exe)
	err := syscall.Exec(exe, os.Args, os.Environ())
	log.Error("cannot restart the Worker with the new binary; start it again by hand", "error", err.Error())
	os.Exit(1)
}

// restartNow is restartProcess: Unix replaces the process in place either way
// (same PID, same terminal and signals), so no Worker is left running behind
// the shell prompt; the lifecycle lock is released before (and its descriptor
// is close-on-exec).
func restartNow(log *slog.Logger, exe string) { restartProcess(log, exe) }

// restartDetached is restartProcess as well.
func restartDetached(log *slog.Logger, exe string) { restartProcess(log, exe) }

// waitForParent is only needed on Windows: elsewhere the process replaces
// itself in place (syscall.Exec).
func waitForParent() {}
