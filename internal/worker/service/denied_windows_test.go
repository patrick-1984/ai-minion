//go:build windows

package service

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// isAccessDenied matches the error number, wrapped as the service manager and
// kardianos wrap it, and nothing else.
func TestIsAccessDeniedMatchesTheErrno(t *testing.T) {
	denied := fmt.Errorf("install the service: %w", syscall.Errno(windows.ERROR_ACCESS_DENIED))
	if !isAccessDenied(denied) {
		t.Errorf("isAccessDenied(%v) = false", denied)
	}
	for _, other := range []error{
		syscall.Errno(windows.ERROR_SERVICE_EXISTS),
		errors.New("Access is denied."),
		nil,
	} {
		if isAccessDenied(other) {
			t.Errorf("isAccessDenied(%v) = true", other)
		}
	}
}
