//go:build !windows

package service

func endUnderSCM(fn func()) error { fn(); return nil }
