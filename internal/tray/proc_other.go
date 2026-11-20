//go:build !windows

package tray

import "os/exec"

func hideWindow(*exec.Cmd) {}
