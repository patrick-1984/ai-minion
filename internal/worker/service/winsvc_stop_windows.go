//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// stopServiceSCM asks the service control manager to stop the Worker's
// service and polls its raw state until it is STOPPED (or gone), all under
// ctx. Unlike kardianos' Stop it never blocks past ctx, and it does not take a
// service that is still stopping for a stopped one.
func stopServiceSCM(ctx context.Context) (wasRunning bool, err error) {
	m, err := mgr.Connect()
	if err != nil {
		return false, err
	}
	defer func() { _ = m.Disconnect() }()
	s, err := m.OpenService(WindowsServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = s.Close() }()
	st, err := s.Query()
	if err != nil {
		return false, err
	}
	if st.State == svc.Stopped {
		return false, nil
	}
	wasRunning = st.State != svc.StopPending
	if wasRunning {
		if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return wasRunning, err
		}
	}
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		st, err := s.Query()
		if err != nil {
			if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
				return wasRunning, nil
			}
			return wasRunning, err
		}
		if st.State == svc.Stopped {
			return wasRunning, nil
		}
		select {
		case <-ctx.Done():
			return wasRunning, fmt.Errorf("the service %s did not stop in time (state %d); stop it (Stop-Service %s) and run the command again", WindowsServiceName, st.State, WindowsServiceName)
		case <-t.C:
		}
	}
}
