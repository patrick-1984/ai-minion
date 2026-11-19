//go:build !windows

package service

import (
	"context"
	"errors"
)

var errNoWindows = errors.New("the Run key exists only on Windows")

// SetAutostart has no Run key outside Windows.
func SetAutostart(string) error { return errNoWindows }

// Autostart reports no Run entry outside Windows.
func Autostart() (string, bool, error) { return "", false, nil }

// DeleteAutostart has nothing to remove outside Windows.
func DeleteAutostart() error { return nil }

// Elevation describes the token of this process (Windows only).
type Elevation struct {
	Admin, Elevated bool
}

// CurrentElevation is never elevated outside Windows.
func CurrentElevation() Elevation { return Elevation{} }

// RemoveLegacyService has no Windows service to remove outside Windows.
func RemoveLegacyService(context.Context) error { return nil }
