//go:build windows

package service

import (
	"context"
	"errors"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// SetAutostart writes cmd as the Worker's value in the user's Run key.
func SetAutostart(cmd string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, AutostartRunKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()
	return k.SetStringValue(AutostartValueName, cmd)
}

// Autostart returns the Worker's value in the user's Run key; ok is false
// when there is none.
func Autostart() (cmd string, ok bool, err error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, AutostartRunKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetStringValue(AutostartValueName)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	return v, err == nil, err
}

// DeleteAutostart removes the Worker's value from the user's Run key.
func DeleteAutostart() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, AutostartRunKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()
	if err := k.DeleteValue(AutostartValueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// Elevation describes the token of this process.
type Elevation struct {
	// Admin is true when the token has administrator rights now.
	Admin bool
	// Elevated is true when it is the elevated half of a UAC split token:
	// the same user also has a normal, unelevated token, which is the one the
	// Worker must run with.
	Elevated bool
}

// CurrentElevation reads the token of this process.
func CurrentElevation() Elevation {
	tok := windows.GetCurrentProcessToken()
	e := Elevation{Admin: tok.IsElevated()}
	var t uint32
	var n uint32
	if err := windows.GetTokenInformation(tok, windows.TokenElevationType, (*byte)(unsafe.Pointer(&t)), uint32(unsafe.Sizeof(t)), &n); err == nil {
		e.Elevated = t == 2 // TokenElevationTypeFull
	}
	return e
}

// RemoveLegacyService removes the LocalSystem service MinionWorker of
// releases up to 0.13.1 (bounded by ctx). It first disables the service, so
// that a queued recovery action cannot start it again, then stops it and
// confirms the stop, deletes it and waits until the service control manager
// no longer knows it. A removal that is only marked for deletion returns
// [ErrLegacyRemovalPending]; a service that cannot be confirmed stopped is an
// error. It needs an administrator.
func RemoveLegacyService(ctx context.Context) error {
	m, err := mgr.Connect()
	if err != nil {
		return windowsInstallError("remove", err)
	}
	defer func() { _ = m.Disconnect() }()
	s, err := m.OpenService(WindowsServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return windowsInstallError("remove", err)
	}
	defer func() { _ = s.Close() }()
	cfg, err := s.Config()
	if err != nil {
		return windowsInstallError("read the configuration of", err)
	}
	if cfg.StartType != mgr.StartDisabled {
		cfg.StartType = mgr.StartDisabled
		if err := s.UpdateConfig(cfg); err != nil {
			return windowsInstallError("disable", err)
		}
	}
	notStopped := errors.New("the service " + WindowsServiceName + " did not stop; " + LegacyRemovalCommand)
	st, err := s.Query()
	if err != nil {
		return windowsInstallError("query", err)
	}
	if st.State != svc.Stopped && st.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return windowsInstallError("stop", err)
		}
	}
	for {
		st, err := s.Query()
		if err != nil {
			return windowsInstallError("query", err)
		}
		if st.State == svc.Stopped {
			break
		}
		select {
		case <-ctx.Done():
			return notStopped
		case <-time.After(250 * time.Millisecond):
		}
	}
	if err := s.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return windowsInstallError("delete", err)
	}
	if st, err := s.Query(); err != nil || st.State != svc.Stopped {
		return notStopped
	}
	_ = s.Close()
	for {
		h, err := m.OpenService(WindowsServiceName)
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil
		}
		if err == nil {
			_ = h.Close()
		}
		select {
		case <-ctx.Done():
			return ErrLegacyRemovalPending
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// CheckAutostartWritable opens the user's Run key for writing, creating
// nothing but the key, and changes no value.
func CheckAutostartWritable() error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, AutostartRunKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	return k.Close()
}
