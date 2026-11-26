//go:build linux

package tmux

import "os"

// bootIDPath is the kernel's random id of the current boot (a UUID).
const bootIDPath = "/proc/sys/kernel/random/boot_id"

// readBootID returns the boot id of the running kernel, "" when it cannot be
// read (a restricted /proc, an SELinux policy).
func readBootID() string {
	b, err := os.ReadFile(bootIDPath)
	if err != nil || len(b) > 128 {
		return ""
	}
	return string(b)
}
