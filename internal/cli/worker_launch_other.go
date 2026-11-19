//go:build !windows

package cli

import (
	"context"
	"errors"
)

// launchWorker exists on Windows only: elsewhere a service manager (systemd,
// launchd) supervises the Worker.
func launchWorker(context.Context, string, []string) error {
	return errors.New("minion worker launch is the Windows logon supervisor; use minion worker up")
}

// startLauncher exists on Windows only.
func startLauncher(string, []string) error {
	return errors.New("the Worker supervisor runs on Windows only")
}
