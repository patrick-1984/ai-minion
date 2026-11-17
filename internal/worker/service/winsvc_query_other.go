//go:build !windows

package service

// QueryWindowsService reports no Windows service on other platforms.
func QueryWindowsService() (WindowsServiceState, error) { return WindowsServiceState{}, nil }
