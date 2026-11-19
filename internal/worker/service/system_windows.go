//go:build windows

package service

import "golang.org/x/sys/windows"

// IsSystemAccount reports whether the token of this process belongs to
// LocalSystem (S-1-5-18), by its user SID. A LocalSystem process has no
// linked UAC token, so the elevation check does not catch it.
func IsSystemAccount() bool {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return true // fail closed: an unreadable token is not trusted
	}
	return u.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
}
