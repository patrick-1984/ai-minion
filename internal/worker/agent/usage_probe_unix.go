//go:build unix

package agent

import (
	"os/exec"
	"syscall"
)

// probeInGroup starts a harness version probe in its own process group, so
// a timeout ends the whole group and not only the direct child.
func probeInGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// endProbeGroup ends what a version probe left running in its group, such as
// a helper a harness starts in the background, so a scan leaves no process
// behind on the user's machine.
func endProbeGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
