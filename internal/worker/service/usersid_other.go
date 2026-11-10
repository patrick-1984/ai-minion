//go:build !windows

package service

import "errors"

// errNotWindows is what [CurrentUserSID] returns away from Windows.
var errNotWindows = errors.New("user SIDs exist on Windows only")

// CurrentUserSID returns an error: user SIDs exist on Windows only.
func CurrentUserSID() (string, error) { return "", errNotWindows }
