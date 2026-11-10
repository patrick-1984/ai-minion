//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package agent

import "os"

// lockFileNB is a no-op on systems that Minion does not ship for (the Worker
// targets Linux, macOS and Windows): there the lifecycle lock does not
// exclude anyone and `minion worker reset` falls back on its control socket
// probe.
func lockFileNB(*os.File) error { return nil }

func unlockFile(*os.File) error { return nil }
