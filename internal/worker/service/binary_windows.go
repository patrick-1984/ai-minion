//go:build windows

package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// serviceDirSDDL is the security descriptor of %ProgramFiles%\Minion: owned by
// the administrators, a protected DACL (nothing inherited from Program Files)
// that gives SYSTEM and the administrators full control and the users read
// and execute (0x1200a9 = FILE_GENERIC_READ | FILE_GENERIC_EXECUTE), all of it
// inherited by every file and subdirectory created in it.
const serviceDirSDDL = "O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)"

// serviceFileSDDL is the same for the binary itself, set explicitly so that a
// file that was already there does not keep a DACL of its own.
const serviceFileSDDL = "O:BAD:PAI(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)"

// programFiles returns the Program Files directory of the machine from the
// known-folder API, not from %ProgramFiles%, which the environment of the
// calling user could change.
func programFiles() (string, error) {
	return windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
}

// ServiceBinaryLocation returns %ProgramFiles%\Minion\minion.exe.
func ServiceBinaryLocation() (string, error) {
	pf, err := programFiles()
	if err != nil {
		return "", fmt.Errorf("cannot find the Program Files directory: %w", err)
	}
	return ServiceBinaryPath(pf), nil
}

// installServiceBinary copies src to %ProgramFiles%\Minion\minion.exe (see
// [installServiceBinaryIn]) and returns that path.
func installServiceBinary(src string) (string, error) {
	pf, err := programFiles()
	if err != nil {
		return "", fmt.Errorf("cannot find the Program Files directory: %w", err)
	}
	return installServiceBinaryIn(src, ServiceBinaryDir(pf))
}

// installServiceBinaryIn makes dir a directory only SYSTEM and the
// administrators can change (created with that DACL, or given it when it
// exists), copies src into it as minion.exe unless src is that file already,
// protects the file the same way and checks the result: an install that
// leaves the binary writable by anyone else fails.
func installServiceBinaryIn(src, dir string) (string, error) {
	dst := dir + `\` + serviceBinaryName
	if err := secureServiceDir(dir); err != nil {
		return "", err
	}
	if !SameWindowsPath(src, dst) {
		if err := copyServiceBinary(src, dst); err != nil {
			return "", err
		}
	}
	if err := applySDDL(dst, serviceFileSDDL); err != nil {
		return "", fmt.Errorf("protect %s: %w", dst, err)
	}
	for _, step := range BinaryChain(dst)[:2] { // the file and its directory
		sd, err := readSecurity(step.Path)
		if err != nil {
			return "", err
		}
		if who := UntrustedWriters(sd, step.Role); len(who) > 0 {
			return "", fmt.Errorf("%s is still writable by %v after the install; refusing to register it for the service", step.Path, who)
		}
	}
	return dst, nil
}

// secureServiceDir creates dir with [serviceDirSDDL], or, when it exists,
// checks that it is a plain directory (not a link or junction) and sets that
// security descriptor on it. Setting a protected DACL on an existing directory
// also replaces what its entries inherited.
func secureServiceDir(dir string) error {
	sd, err := windows.SecurityDescriptorFromString(serviceDirSDDL)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	err = windows.CreateDirectory(p, sa)
	if err == nil {
		return nil
	}
	if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return &os.PathError{Op: "stat", Path: dir, Err: err}
	}
	if attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("%s exists and is not a directory", dir)
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s is a symbolic link or junction; remove it and run the command again", dir)
	}
	return applySDDL(dir, serviceDirSDDL)
}

// applySDDL sets the owner and the protected DACL of sddl on path.
func applySDDL(path, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return errors.New("internal error: no DACL in " + sddl)
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, nil, dacl, nil)
	if err != nil {
		return &os.PathError{Op: "set security", Path: path, Err: err}
	}
	return nil
}

// copyServiceBinary copies src next to dst (so the copy inherits the DACL of
// the directory) and renames it over dst. A dst that still runs (a service
// that did not stop, a console Worker started from it) cannot be deleted on
// Windows but can be renamed: it is moved to dst.old first and removed when it
// no longer runs.
func copyServiceBinary(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if fi, err := in.Stat(); err != nil {
		return err
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".minion-install-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	old := dst + ".old"
	if _, err := os.Lstat(dst); err == nil {
		if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
			_ = os.Rename(old, fmt.Sprintf("%s-%d", old, time.Now().UnixNano()))
		}
		if err := os.Rename(dst, old); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Rename(old, dst)
		return err
	}
	done = true
	_ = os.Remove(old) // fails while it still runs; a later install removes it
	return nil
}

// readSecurity reads the owner and DACL of path.
func readSecurity(path string) (SecurityInfo, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return SecurityInfo{}, &os.PathError{Op: "get security", Path: path, Err: err}
	}
	return securityInfoOf(sd)
}

// securityInfoOf converts a security descriptor to [SecurityInfo].
func securityInfoOf(sd *windows.SECURITY_DESCRIPTOR) (SecurityInfo, error) {
	var out SecurityInfo
	if owner, _, err := sd.Owner(); err == nil && owner != nil {
		out.Owner = owner.String()
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
			out.NullDACL = true
			return out, nil
		}
		return out, err
	}
	if dacl == nil {
		out.NullDACL = true
		return out, nil
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return out, err
		}
		a := ACE{Type: ace.Header.AceType, Flags: ace.Header.AceFlags, Mask: uint32(ace.Mask)}
		if a.Type == aceAllowed || a.Type == aceDenied {
			a.SID = (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		}
		out.DACL = append(out.DACL, a)
	}
	return out, nil
}

// CheckServiceBinary returns the principals other than SYSTEM, the
// administrators and TrustedInstaller that could replace binary: through the
// file, its directory or a directory above it.
func CheckServiceBinary(binary string) ([]BinaryFinding, error) {
	f, err := FindingsFor(binary, readSecurity)
	for i := range f {
		if sid, serr := windows.StringToSid(f[i].SID); serr == nil {
			if acct, dom, _, lerr := sid.LookupAccount(""); lerr == nil {
				if dom != "" {
					acct = dom + `\` + acct
				}
				f[i].Name = acct
			}
		}
	}
	return f, err
}

// ServiceBinaryProblem looks at the binary the Worker's Windows service is
// registered with and returns the problem line of "minion worker status" when
// a non-administrator could replace it; empty when the service is not
// installed or the binary is safe. A check that cannot run is reported as
// such, not as safe.
func ServiceBinaryProblem() string {
	st, err := QueryWindowsService()
	if err != nil || !st.Installed || st.BinaryPath == "" {
		return ""
	}
	findings, err := CheckServiceBinary(st.BinaryPath)
	if err != nil && len(findings) == 0 {
		return fmt.Sprintf("cannot check who may change the service binary %s: %v", st.BinaryPath, err)
	}
	return ServiceBinaryWarning(st.BinaryPath, findings)
}

// IsElevated reports whether this process runs with an elevated
// (administrator) token.
func IsElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }
