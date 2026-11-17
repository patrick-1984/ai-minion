//go:build !windows

package agent

import "os"

// workerRunMode is how this Worker runs: under systemd, under launchd or in
// the foreground.
func workerRunMode() string { return unixRunMode(os.Getenv) }
