//go:build !linux && !windows

package tmux

import "syscall"

func ctimeNano(st *syscall.Stat_t) int64 { return st.Ctimespec.Nano() }
