//go:build windows

package cli

// runningAsRoot is false on Windows: the Worker state is per user there and
// the service install path reads it as that user.
func runningAsRoot() bool { return false }
