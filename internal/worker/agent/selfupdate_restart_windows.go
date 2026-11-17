//go:build windows

package agent

import (
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"time"

	"golang.org/x/sys/windows/svc"
)

// waitPIDEnv names the process a restarted Worker waits for (foreground).
const waitPIDEnv = "MINION_UPDATE_WAIT_PID"

// underServiceManager reports whether the Windows service control manager runs
// this process.
func underServiceManager() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// restartProcess starts the new binary. A service exits with a non-zero code:
// the service's recovery action (restart after 5 s) starts it again. In the
// foreground the Worker stops and the command starts the new binary in the same
// console and waits for it (v0.9.3); without a running runtime the new binary is
// started with the same arguments and console, then this process exits.
func restartProcess(log *slog.Logger, exe string) {
	if underServiceManager() {
		log.Info("restarting the Worker", "path", "service", "how", "exit with the restart code; the service recovery starts the new binary", "exe", exe)
		os.Exit(exitRestart)
	}
	// Foreground: stop cleanly and let the command restart the new binary in
	// this console and wait for it (worker run, RestartPending), so the console
	// keeps owning the Worker and Ctrl+C stops it.
	if requestRestart(exe) {
		log.Info("restarting the Worker", "path", "console", "how", "stop, then the command runs the new binary in this console", "exe", exe)
		return
	}
	log.Info("restarting the Worker", "path", "detached", "how", "no running runtime; start the new binary and exit", "exe", exe)
	restartNow(log, exe)
}

// restartNow starts the binary with the same arguments and console and exits:
// the start-up rollback, where nothing runs yet that could stop cleanly. The
// new process waits for this one to exit (waitForParent).
func restartNow(log *slog.Logger, exe string) {
	if underServiceManager() {
		os.Exit(exitRestart)
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The new process waits for this one to exit before it starts its
	// modules: the local control pipe is exclusive (waitForParent).
	cmd.Env = append(os.Environ(), waitPIDEnv+"="+strconv.Itoa(os.Getpid()))
	log.Debug("starting the new Worker binary detached, with the same console", "exe", exe, "args", len(os.Args)-1)
	if err := cmd.Start(); err != nil {
		log.Error("cannot restart the Worker with the new binary; start it again by hand", "error", err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}

// waitForParent waits (up to 30 s) for the process that started this one for
// an update to exit, so that its exclusive resources are free.
func waitForParent() {
	v := os.Getenv(waitPIDEnv)
	if v == "" {
		return
	}
	_ = os.Unsetenv(waitPIDEnv)
	pid, err := strconv.Atoi(v)
	if err != nil {
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	done := make(chan struct{})
	go func() { _, _ = p.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
}
