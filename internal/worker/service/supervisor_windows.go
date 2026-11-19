//go:build windows

package service

import (
	"errors"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// supervisorSuffix is "-" and the SID of the user of this process.
func supervisorSuffix() string {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return ""
	}
	return "-" + u.User.Sid.String()
}

// supervisorAttrs returns SECURITY_ATTRIBUTES whose DACL grants this user and
// SYSTEM only, so that no other account can open, signal or hold the objects.
func supervisorAttrs() (*windows.SecurityAttributes, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;GA;;;" + u.User.Sid.String() + ")")
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, nil
}

// AcquireSupervisor takes the ownership of the supervisor role of this user.
// already is true when another supervisor holds it. release gives it up.
func AcquireSupervisor() (release func(), already bool, err error) {
	sa, err := supervisorAttrs()
	if err != nil {
		return nil, false, err
	}
	name, err := windows.UTF16PtrFromString(SupervisorMutexName())
	if err != nil {
		return nil, false, err
	}
	m, err := windows.CreateMutex(sa, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(m)
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return func() { _ = windows.CloseHandle(m) }, false, nil
}

// SupervisorRunning reports whether a supervisor of this user holds its mutex.
func SupervisorRunning() bool {
	name, err := windows.UTF16PtrFromString(SupervisorMutexName())
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

// SignalSupervisorStop asks the supervisor of this user to stop: it ends
// pending restarts, stops the Worker and exits. A supervisor that holds the
// mutex but has not published its stop event yet (it is still starting) is
// waited for. found is false when no supervisor runs.
func SignalSupervisorStop() (found bool, err error) {
	name, err := windows.UTF16PtrFromString(SupervisorStopEvent())
	if err != nil {
		return false, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if !SupervisorRunning() {
			return false, nil
		}
		h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
		if err == nil {
			defer func() { _ = windows.CloseHandle(h) }()
			return true, windows.SetEvent(h)
		}
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return false, err
		}
		if time.Now().After(deadline) {
			return true, errors.New("the supervisor did not publish its stop event within 10 seconds")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// CreateSupervisorStopEvent creates the event that [SignalSupervisorStop]
// sets; stop is closed when it is set. closeFn ends the waiter and closes the
// event; call it before releasing the supervisor role, so that the next
// supervisor does not find this one's signaled event.
func CreateSupervisorStopEvent() (stop <-chan struct{}, closeFn func(), err error) {
	sa, err := supervisorAttrs()
	if err != nil {
		return nil, nil, err
	}
	name, err := windows.UTF16PtrFromString(SupervisorStopEvent())
	if err != nil {
		return nil, nil, err
	}
	h, err := windows.CreateEvent(sa, 1, 0, name)
	if h == 0 {
		return nil, nil, err
	}
	ch := make(chan struct{})
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			ev, _ := windows.WaitForSingleObject(h, 200)
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
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			close(quit)
			<-done
			_ = windows.CloseHandle(h)
		})
	}, nil
}
