//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package version

import "os"

// installLockNB is a no-op on systems that Minion does not ship for (the Worker
// targets Linux, macOS and Windows): there the lifecycle lock does not
// exclude anyone and `minion worker reset` falls back on its control socket
// probe.
func installLockNB(*os.File) error { return nil }

func installUnlock(*os.File) error { return nil }
