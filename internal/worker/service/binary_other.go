//go:build !windows

package service

import "errors"

// errNoWindows is returned by the Windows-only parts of the service binary
// outside Windows.
var errNoWindows = errors.New("the Windows service binary exists only on Windows")

// installServiceBinary leaves src where it is outside Windows: no other
// platform installs a Windows service (the tests replace this step).
func installServiceBinary(src, _ string) (string, error) { return src, nil }

// ServiceBinaryLocation has no answer outside Windows.
func ServiceBinaryLocation() (string, error) { return "", errNoWindows }

// CheckServiceBinary has nothing to check outside Windows.
func CheckServiceBinary(string) ([]BinaryFinding, error) { return nil, errNoWindows }

// ServiceBinaryProblem reports nothing outside Windows.
func ServiceBinaryProblem() string { return "" }

// IsElevated is false outside Windows.
func IsElevated() bool { return false }
