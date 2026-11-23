package agentdetect

import (
	"os"
	"strconv"
	"strings"
)

// ProcessBirth identifies the lifetime of a live process on this host. The
// return value contains no process arguments and is empty when unprovable.
func ProcessBirth(pid int) string {
	if pid <= 1 {
		return ""
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	text := string(stat)
	i := strings.LastIndexByte(text, ')')
	if i < 0 {
		return ""
	}
	fields := strings.Fields(text[i+1:])
	// Field 22 is process start in clock ticks since boot. The state is
	// field 3 and begins this suffix.
	if len(fields) <= 19 || !(Proc{State: fields[0]}).Running() {
		return ""
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || ticks == 0 {
		return ""
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	bootID := strings.TrimSpace(string(boot))
	if err != nil || len(bootID) != 36 {
		return ""
	}
	return strconv.Itoa(pid) + ":" + bootID + ":" + fields[19]
}
