//go:build !windows

package config

import (
	"errors"
	"os"
	"syscall"
)

// syncDirFn flushes a directory; tests replace it.
var syncDirFn = syncDir

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
