//go:build !windows

package service

import "errors"

// SupervisorRunning is false outside Windows.
func SupervisorRunning() bool { return false }

// SignalSupervisorStop finds no supervisor outside Windows.
func SignalSupervisorStop() (bool, error) { return false, nil }

// CreateSupervisorStopEvent exists only on Windows.
func CreateSupervisorStopEvent() (<-chan struct{}, func(), error) {
	return nil, nil, errors.New("the supervisor stop event exists only on Windows")
}
