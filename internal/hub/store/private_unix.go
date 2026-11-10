//go:build !windows

package store

import "os"

// createPrivate creates the new file p, which must not exist, for writing
// with mode 0600. The mode applies at creation, before any byte is written.
func createPrivate(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

// mkdirAllPrivate creates the directory p and its missing parents with mode
// 0700.
func mkdirAllPrivate(p string) error {
	return os.MkdirAll(p, 0o700)
}

// mkdirTempPrivate creates a new directory in dir (pattern as in
// [os.MkdirTemp]) that only the owner can enter; os.MkdirTemp uses mode 0700.
func mkdirTempPrivate(dir, pattern string) (string, error) {
	return os.MkdirTemp(dir, pattern)
}

// protectDir restricts the directory p, which this process just created, to
// its owner. On Unix the creation mode already did that.
func protectDir(string) error { return nil }
