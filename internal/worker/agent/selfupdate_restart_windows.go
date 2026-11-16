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

// underServiceManager reports whether the Windows service control manager runs
// this process.
func underServiceManager() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// restartProcess starts the new binary. A service exits with a non-zero code:
// the service's recovery action (restart after 5 s) starts it again. In the
// foreground the new binary is started with the same arguments and console,
// then this process exits.
func restartProcess(log *slog.Logger, exe string) {
	if underServiceManager() {
		os.Exit(exitRestart)
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The new process waits for this one to exit before it starts its
	// modules: the local control pipe is exclusive (waitForParent).
	cmd.Env = append(os.Environ(), waitPIDEnv+"="+strconv.Itoa(os.Getpid()))
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
