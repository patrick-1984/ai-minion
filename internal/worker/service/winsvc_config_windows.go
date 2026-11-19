//go:build windows

package service

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	ksvc "github.com/kardianos/service"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/mgr"
)

// reconfigureService changes the existing Worker service in place to run cfg
// (binary, arguments, start type, recovery) with env, instead of deleting and
// creating it: a deletion can hang in "marked for deletion" while any program
// holds a handle to the service, and leave no service at all. restore puts
// the previous configuration and environment back.
func reconfigureService(cfg *ksvc.Config, env map[string]string) (restore func() error, err error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, err
	}
	defer func() { _ = m.Disconnect() }()
	s, err := m.OpenService(WindowsServiceName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	old, err := s.Config()
	if err != nil {
		return nil, err
	}
	oldEnv, hadEnv, err := readServiceEnvironment()
	if err != nil {
		return nil, err
	}
	next := old
	next.BinaryPathName = serviceCommandLine(cfg.Executable, cfg.Arguments)
	next.StartType = mgr.StartAutomatic
	next.ServiceType = windows.SERVICE_WIN32_OWN_PROCESS
	next.ErrorControl = mgr.ErrorNormal
	next.DisplayName = cfg.DisplayName
	next.Description = cfg.Description
	next.ServiceStartName = "LocalSystem"
	next.Password = ""
	next.Dependencies = nil
	if err := s.UpdateConfig(next); err != nil {
		return nil, err
	}
	restore = func() error {
		var errs []error
		if err := updateConfigByName(old); err != nil {
			errs = append(errs, err)
		}
		if hadEnv {
			errs = append(errs, writeServiceEnvironment(oldEnv))
		} else {
			errs = append(errs, deleteServiceEnvironment())
		}
		return errors.Join(errs...)
	}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}}, failureResetPeriodSeconds); err != nil {
		_ = restore()
		return nil, err
	}
	if err := setServiceEnvironment(env); err != nil {
		_ = restore()
		return nil, err
	}
	return restore, nil
}

func updateConfigByName(c mgr.Config) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer func() { _ = m.Disconnect() }()
	s, err := m.OpenService(WindowsServiceName)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	c.Password = ""
	return s.UpdateConfig(c)
}

// serviceCommandLine quotes the executable and the arguments as Windows
// parses them.
func serviceCommandLine(exe string, args []string) string {
	parts := []string{`"` + exe + `"`}
	for _, a := range args {
		parts = append(parts, syscall.EscapeArg(a))
	}
	return strings.Join(parts, " ")
}

func readServiceEnvironment() ([]string, bool, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, serviceKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetStringsValue("Environment")
	if errors.Is(err, registry.ErrNotExist) {
		return nil, false, nil
	}
	return v, err == nil, err
}

func writeServiceEnvironment(v []string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, serviceKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()
	return k.SetStringsValue("Environment", v)
}

func deleteServiceEnvironment() error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, serviceKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()
	if err := k.DeleteValue("Environment"); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// minionKeyPath holds what only administrators may record about the
// installation: the identity of the service binary directory this installer
// created.
const minionKeyPath = `SOFTWARE\Minion`

// dirIdentity returns the volume serial number and file index of dir (not
// following a reparse point): it names that very directory, not whatever is
// at its path later.
func dirIdentity(dir string) (string, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return "", &os.PathError{Op: "open", Path: dir, Err: err}
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("%08x-%08x%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}

// recordedServiceDir returns the identity recorded for the service directory
// ("" when none).
func recordedServiceDir() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, minionKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetStringValue("ServiceDirID")
	if err != nil {
		return ""
	}
	return v
}

func recordServiceDir(id string) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, minionKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()
	return k.SetStringValue("ServiceDirID", id)
}

// stagingSDDL protects a staging directory: SYSTEM and the administrators
// only, nothing inherited.
const stagingSDDL = "O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// NewStagingDir creates a fresh directory below Program Files that only
// SYSTEM and the administrators can open, secured at creation, for an
// elevated download and verification of a release (the update cache of the
// user is open to the user's programs). cleanup removes it.
func NewStagingDir() (dir string, cleanup func(), err error) {
	pf, err := programFiles()
	if err != nil {
		return "", nil, err
	}
	for _, step := range BinaryChain(pf + `\x\x`)[2:] {
		if err := checkStep(step); err != nil {
			return "", nil, err
		}
	}
	sd, err := windows.SecurityDescriptorFromString(stagingSDDL)
	if err != nil {
		return "", nil, err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	dir = fmt.Sprintf(`%s\%s.staging-%x`, strings.TrimRight(pf, `\`), ServiceBinaryDirName, b)
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return "", nil, err
	}
	if err := windows.CreateDirectory(p, sa); err != nil {
		return "", nil, &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}
