//go:build !windows

package service

// IsSystemAccount is false outside Windows (LocalSystem is a Windows account).
func IsSystemAccount() bool { return false }
