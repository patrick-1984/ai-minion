//go:build !linux && !darwin

package filexfer

import (
	"errors"
	"os"
	"time"
)

// Platforms without a transfer end in files.v1 (Windows, BRD 3.2 and 5.3):
// the formats, codings and manifest work, every file system primitive fails
// with errUnsupported.

const sysSupported = false

func sysStatAt(int, string) (statInfo, error)           { return statInfo{}, errUnsupported }
func sysFstat(int) (statInfo, error)                    { return statInfo{}, errUnsupported }
func sysCreateExcl(int, string) (*os.File, error)       { return nil, errUnsupported }
func sysSetMTime(int, time.Time) error                  { return errUnsupported }
func sysRenameNoReplace(int, string, int, string) error { return errUnsupported }
func sysRenameReplace(int, string, int, string) error   { return errUnsupported }
func sysMkdirAt(int, string) error                      { return errUnsupported }
func sysUnlinkAt(int, string) error                     { return errUnsupported }
func sysFreeBytes(int) (uint64, uint64, error)          { return 0, 0, errUnsupported }
func sysUmask() uint32                                  { return 0o022 }
func sysOpenRootDir(string) (*os.File, error)           { return nil, errUnsupported }

func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
func isNoSpace(error) bool      { return false }
func isLoop(error) bool         { return false }
func isXDev(error) bool         { return false }

// openAbsNoFollow (cleanup.go) is used by the Linux and macOS files only.
var _ = openAbsNoFollow
