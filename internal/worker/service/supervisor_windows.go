//go:build windows

package service

import (
	"errors"

	"golang.org/x/sys/windows"
)

// SupervisorRunning reports whether a supervisor ("worker launch") of this
// logon session holds its mutex.
func SupervisorRunning() bool {
	name, err := windows.UTF16PtrFromString(SupervisorMutexName)
	if err != nil {
		return false
	}
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, name)
	if err != nil {
		return false
	}
	_ = windows.CloseHandle(h)
	return true
}

// SignalSupervisorStop asks the supervisor of this session to stop: it ends
// pending restarts, stops the Worker and exits. found is false when no
// supervisor listens.
func SignalSupervisorStop() (found bool, err error) {
	name, err := windows.UTF16PtrFromString(SupervisorStopEvent)
	if err != nil {
		return false, err
	}
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return true, windows.SetEvent(h)
}

// CreateSupervisorStopEvent creates the event that [SignalSupervisorStop]
// sets; the returned wait blocks until it is set.
func CreateSupervisorStopEvent() (stop <-chan struct{}, closeFn func(), err error) {
	name, err := windows.UTF16PtrFromString(SupervisorStopEvent)
	if err != nil {
		return nil, nil, err
	}
	h, err := windows.CreateEvent(nil, 1, 0, name)
	if h == 0 {
		return nil, nil, err
	}
	ch := make(chan struct{})
	quit := make(chan struct{})
	go func() {
		for {
			ev, _ := windows.WaitForSingleObject(h, 500)
			if ev == windows.WAIT_OBJECT_0 {
				close(ch)
				return
			}
			select {
			case <-quit:
				return
			default:
			}
		}
	}()
	return ch, func() {
		close(quit)
		// The goroutine may still wait on h for up to 500 ms; the handle
		// is closed with the process.
	}, nil
}
