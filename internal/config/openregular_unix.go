//go:build unix

package config

import "syscall"

const nonBlockFlag = syscall.O_NONBLOCK
