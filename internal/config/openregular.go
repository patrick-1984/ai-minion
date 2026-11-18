package config

import (
	"fmt"
	"os"
)

// openRegular opens path for reading without blocking on a FIFO or a device
// (non-blocking open where the system has it) and refuses anything that is
// not a regular file. The error names the path, never the content.
func openRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|nonBlockFlag, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return f, nil
}
