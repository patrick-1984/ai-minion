//go:build !windows

package service

import "context"

// stopServiceSCM has no Windows service to stop outside Windows.
func stopServiceSCM(context.Context) (bool, error) { return false, nil }
