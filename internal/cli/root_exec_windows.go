//go:build windows

package cli

// checkRootExecutable has nothing to check on Windows (no root Worker).
func checkRootExecutable(string) error { return nil }
