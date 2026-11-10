//go:build !windows

package identity

import "syscall"

func umask(m int) int { return syscall.Umask(m) }
