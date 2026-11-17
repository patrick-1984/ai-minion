//go:build windows

package service

import (
	"errors"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// QueryWindowsService looks at the Worker's Windows service with query rights
// only (SC_MANAGER_CONNECT, SERVICE_QUERY_STATUS | SERVICE_QUERY_CONFIG), so a
// user without administrator rights can ask. It never changes the SCM. A
// service that does not exist is reported as not installed, not as an error.
func QueryWindowsService() (WindowsServiceState, error) {
	var st WindowsServiceState
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return st, err
	}
	defer func() { _ = windows.CloseServiceHandle(scm) }()
	name, err := windows.UTF16PtrFromString(WindowsServiceName)
	if err != nil {
		return st, err
	}
	h, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	s := &mgr.Service{Name: WindowsServiceName, Handle: h}
	defer func() { _ = s.Close() }()
	st.Installed = true
	if status, err := s.Query(); err == nil {
		st.Running = status.State == svc.Running || status.State == svc.StartPending
	}
	if cfg, err := s.Config(); err == nil {
		st.AutoStart = cfg.StartType == mgr.StartAutomatic
		st.Account = cfg.ServiceStartName
	}
	return st, nil
}
