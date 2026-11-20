//go:build windows

package tmux

// rootSocketDirSafe has nothing to check on Windows (psmux has no Unix socket).
func rootSocketDirSafe(string) error { return nil }
