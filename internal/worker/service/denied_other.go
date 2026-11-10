//go:build !windows

package service

// isAccessDenied is false away from Windows: the error numbers of the service
// control manager exist there only.
func isAccessDenied(error) bool { return false }
