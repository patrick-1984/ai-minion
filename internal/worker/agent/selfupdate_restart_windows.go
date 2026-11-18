//go:build windows

package agent

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
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
	restartDetached(log, exe)
}

// restartNow hands over to the binary at exe: the start-up rollback
// (RecoverUpdate), where nothing runs yet that could stop cleanly. A service
// exits with the restart code. In a console the binary runs with the same
// arguments in this console and this process waits for it, then exits with
// its exit code: the shell gets its prompt back only when the Worker really
// ended, and Ctrl+C reaches the Worker that runs (v0.12.0). Starting it
// detached instead left a Worker running behind the prompt that held the
// state lock. The caller has released the lifecycle lock and the enrollment
// lock; no module runs yet, so nothing else this process holds blocks the new
// Worker, and the new one must not wait for this one (waitForParent).
func restartNow(log *slog.Logger, exe string) {
	if underServiceManager() {
		os.Exit(exitRestart)
	}
	log.Info("restarting the Worker", "path", "console", "how", "run the restored binary in this console and wait for it", "exe", exe)
	os.Exit(runInConsole(log, exe))
}

// runInConsole runs exe with this process's arguments, console and
// environment (without waitPIDEnv) and returns its exit code. This process
// consumes Ctrl+C while it waits: the console delivers it to the child as
// well, which stops; signal.Ignore would not stop Windows' default handler
// from ending this process.
func runInConsole(log *slog.Logger, exe string) int {
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, os.Interrupt)
	go func() {
		for range sig {
		}
	}()
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = consoleChildEnv(os.Environ())
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		log.Error("cannot restart the Worker with the restored binary; start it again by hand", "error", err.Error())
		return 1
	}
	return 0
}

// restartDetached starts the binary with the same arguments and console and
// exits, the way a restart worked before v0.9.3. It is left for the cases in
// which this process may still hold exclusive resources that only its exit
// releases (a stop that did not finish): the new process waits for this one
// to exit before it starts its modules (waitForParent).
func restartDetached(log *slog.Logger, exe string) {
	if underServiceManager() {
		os.Exit(exitRestart)
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(consoleChildEnv(os.Environ()), waitPIDEnv+"="+strconv.Itoa(os.Getpid()))
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
