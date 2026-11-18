//go:build !windows

package service

// serviceKeyGone has no registry to look at outside Windows.
func serviceKeyGone() bool { return true }

// setServiceEnvironment has no registry to write outside Windows.
func setServiceEnvironment(map[string]string) error { return nil }

// removeServiceKey has no registry to change outside Windows.
func removeServiceKey() error { return nil }
