//go:build !windows

package cli

import (
	"io"
	"os"
)

func openReplaceTerminal() (io.ReadWriteCloser, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

func replaceAutostartState() string { return "" }

func saveReplaceRegistration() (func() error, error) {
	return func() error { return nil }, nil
}
