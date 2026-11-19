//go:build !windows

package service

import "errors"

func supervisorSuffix() string { return "" }

// AcquireSupervisor exists only on Windows.
func AcquireSupervisor() (func(), bool, error) {
	return nil, false, errors.New("the Worker supervisor runs on Windows only")
}

// SupervisorRunning is false outside Windows.
func SupervisorRunning() bool { return false }

// SignalSupervisorStop finds no supervisor outside Windows.
func SignalSupervisorStop() (bool, error) { return false, nil }

// CreateSupervisorStopEvent exists only on Windows.
func CreateSupervisorStopEvent() (<-chan struct{}, func(), error) {
	return nil, nil, errors.New("the supervisor stop event exists only on Windows")
}
