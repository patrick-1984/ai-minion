//go:build !windows

package cli

// restartInConsole is not used outside Windows: the Unix Worker replaces its
// own process (syscall.Exec) and keeps its pid and terminal.
func restartInConsole(string) error { return nil }
