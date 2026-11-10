//go:build windows

package service

import "golang.org/x/sys/windows"

// CurrentUserSID returns the SID of the user that runs the process.
func CurrentUserSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}
