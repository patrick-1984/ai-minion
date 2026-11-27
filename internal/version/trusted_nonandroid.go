//go:build !android && !windows

package version

import "os"

func platformTrustedUID(uint32) bool { return false }

func platformTrustedGroupWrite(os.FileInfo) bool { return false }
