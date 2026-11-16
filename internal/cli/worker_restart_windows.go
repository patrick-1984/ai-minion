//go:build windows

package cli

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
)

// restartInConsole runs the new binary of a foreground self-update in this
// console and waits for it, then exits with its exit code (v0.9.3). Ctrl+C
// reaches both processes; this one ignores it and leaves the stop to the new
// Worker, so the console stays with the running Worker.
func restartInConsole(exe string) error {
	signal.Ignore(os.Interrupt)
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitCode())
	}
	if err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
