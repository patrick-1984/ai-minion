//go:build windows

package tray

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW: the tray is a GUI program and minion.exe
// a console program, so without it every poll would flash a console window.
const createNoWindow = 0x08000000

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
