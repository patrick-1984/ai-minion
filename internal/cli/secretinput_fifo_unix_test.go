//go:build unix

package cli

import "syscall"

func syscallMkfifo(p string) error { return syscall.Mkfifo(p, 0o600) }
