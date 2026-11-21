//go:build !linux

package truenas

import (
	"errors"
	"os"
)

var errUnsupported = errors.New("the TrueNAS host executor runs on Linux only")

func openNoFollow(string) (*os.File, error) { return nil, errUnsupported }

func checkOwner(os.FileInfo, int) error { return errUnsupported }

func lockFile(*os.File) error { return errUnsupported }
