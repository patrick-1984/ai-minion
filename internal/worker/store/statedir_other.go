//go:build !windows

package store

// protectStateDir is a no-op off Windows: the state directory is created with
// mode 0700 and SQLite gives the -wal and -shm files the mode of the main file.
func protectStateDir(string) error { return nil }
