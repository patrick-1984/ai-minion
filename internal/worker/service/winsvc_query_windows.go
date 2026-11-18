//go:build windows

package service

import (
	"errors"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// QueryWindowsService looks at the Worker's Windows service with query rights
// only (SC_MANAGER_CONNECT, then SERVICE_QUERY_STATUS and SERVICE_QUERY_CONFIG
// on separate handles), so a user without administrator rights can ask. A
// service ACL that grants one right and not the other still gives the part it
// grants: the running state alone, or the start type and account alone. It
// never changes the SCM. A service that does not exist is reported as not
// installed, not as an error; an error means neither part could be read.
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
	var errs []error
	open := func(access uint32, read func(s *mgr.Service) error) (missing bool) {
		h, err := windows.OpenService(scm, name, access)
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return true
		}
		if err != nil {
			errs = append(errs, err)
			return false
		}
		s := &mgr.Service{Name: WindowsServiceName, Handle: h}
		defer func() { _ = s.Close() }()
		st.Installed = true
		if err := read(s); err != nil {
			errs = append(errs, err)
		}
		return false
	}
	if open(windows.SERVICE_QUERY_STATUS, func(s *mgr.Service) error {
		status, err := s.Query()
		if err == nil {
			st.Running = status.State == svc.Running || status.State == svc.StartPending
		}
		return err
	}) {
		return WindowsServiceState{}, nil
	}
	if open(windows.SERVICE_QUERY_CONFIG, func(s *mgr.Service) error {
		cfg, err := s.Config()
		if err == nil {
			st.AutoStart = cfg.StartType == mgr.StartAutomatic
			st.Account = cfg.ServiceStartName
			st.BinaryPath = ParseServiceImagePath(cfg.BinaryPathName)
		}
		return err
	}) {
		return WindowsServiceState{}, nil
	}
	if !st.Installed && len(errs) > 0 {
		return st, errors.Join(errs...)
	}
	return st, nil
}
