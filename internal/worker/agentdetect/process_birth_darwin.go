package agentdetect

import (
	"strconv"

	"golang.org/x/sys/unix"
)

// ProcessBirth identifies the lifetime of a live process on this host. The
// return value contains no process arguments and is empty when unprovable.
func ProcessBirth(pid int) string {
	if pid <= 1 {
		return ""
	}
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(p.Proc.P_pid) != pid || p.Proc.P_stat == 4 || p.Proc.P_stat == 5 {
		return "" // stopped or zombie (Darwin sys/proc.h)
	}
	start := p.Proc.P_starttime
	if start.Sec <= 0 {
		return ""
	}
	return strconv.Itoa(pid) + ":" + strconv.FormatInt(start.Nano(), 10)
}
