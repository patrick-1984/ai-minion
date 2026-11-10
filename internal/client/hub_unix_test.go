//go:build unix

package client

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A path that names a pipe (or any non-regular file) must be refused at once:
// reading it could block for ever.
func TestHubKeyAndCAFilesThatAreNotRegularFilesDoNotBlock(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 2)
	go func() { _, err := readAPIKey(fifo); done <- err }()
	go func() { _, err := readBoundedFile(fifo, maxCAFileBytes); done <- err }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err == nil {
				t.Error("a pipe was accepted as a file")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("reading a pipe blocked")
		}
	}
	if _, err := readAPIKey("/dev/zero"); !errors.Is(err, ErrAPIKeyFile) {
		t.Errorf("/dev/zero: %v", err)
	}
}
