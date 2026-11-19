//go:build !windows

package service

import (
	"errors"

	ksvc "github.com/kardianos/service"
)

// errNoWindows is returned by the Windows-only parts of the service binary
// outside Windows.
var errNoWindows = errors.New("the Windows service binary exists only on Windows")

// installServiceBinary has no Program Files outside Windows: no other
// platform installs a Windows service (the tests replace this step).
func installServiceBinary(BinaryInstall, func(string) error) (string, error) {
	return "", errNoWindows
}

// NewStagingDir has no Program Files outside Windows.
func NewStagingDir() (string, func(), error) { return "", nil, errNoWindows }

// reconfigureService has no service manager outside Windows.
func reconfigureService(*ksvc.Config, map[string]string) (func() error, error) {
	return nil, errNoWindows
}

// ServiceBinaryLocation has no answer outside Windows.
func ServiceBinaryLocation() (string, error) { return "", errNoWindows }

// CheckServiceBinary has nothing to check outside Windows.
func CheckServiceBinary(string) ([]BinaryFinding, error) { return nil, errNoWindows }

// ServiceBinaryProblem reports nothing outside Windows.
func ServiceBinaryProblem() string { return "" }

// IsElevated is false outside Windows.
func IsElevated() bool { return false }
