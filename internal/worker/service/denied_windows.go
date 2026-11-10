//go:build windows

package service

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isAccessDenied reports whether err is the Windows error ACCESS_DENIED, in
// whatever language the system formats its message.
func isAccessDenied(err error) bool { return errors.Is(err, windows.ERROR_ACCESS_DENIED) }
