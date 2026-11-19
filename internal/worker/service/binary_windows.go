//go:build windows

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/patrick-1984/ai-minion/internal/version"
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

// installServiceBinary puts bi in place as %ProgramFiles%\Minion\minion.exe
// and activates it (see [installServiceBinaryIn]); it returns that path.
func installServiceBinary(bi BinaryInstall, activate func(dst string) error) (string, error) {
	pf, err := programFiles()
	if err != nil {
		return "", fmt.Errorf("cannot find the Program Files directory: %w", err)
	}
	return installServiceBinaryIn(bi, ServiceBinaryDir(pf), true, activate)
}

// dirIdentityStore records which directory this installer created; tests
// replace it (the real one is HKLM, which only administrators may change).
var dirIdentityStore = struct {
	get func() string
	set func(string) error
}{recordedServiceDir, recordServiceDir}

// installServiceBinaryIn makes dir a directory only SYSTEM and the
// administrators have ever been able to change, copies bi into it as
// minion.exe and, before activate runs, protects the file, checks that
// nothing in dir and nothing on the way to it can be changed by anyone else
// and that the path has no symbolic link or junction. ancestors is false only
// in tests, whose temporary directories lie below the test user's profile.
func installServiceBinaryIn(bi BinaryInstall, dir string, ancestors bool, activate func(dst string) error) (string, error) {
	dst := dir + `\` + serviceBinaryName
	chain := BinaryChain(dst)
	if !ancestors {
		chain = chain[:2]
	}
	// The directories above must be safe before anything is created in them.
	for _, step := range chain[2:] {
		if err := checkStep(step); err != nil {
			return "", err
		}
	}
	if err := secureServiceDir(dir); err != nil {
		return "", err
	}
	err := replaceAndActivate(dst, bi, installedTrusted, func(dst string) error {
		if err := applySDDL(dst, serviceFileSDDL); err != nil {
			return fmt.Errorf("protect %s: %w", dst, err)
		}
		for _, step := range chain {
			if err := checkStep(step); err != nil {
				return fmt.Errorf("%w; refusing to register it for the service", err)
			}
		}
		if err := checkEntries(dir); err != nil {
			return err
		}
		return activate(dst)
	})
	if err != nil {
		return "", err
	}
	return dst, nil
}

// installedTrusted reads the release of the protected binary by running it
// ("version"); it is run only when nobody else can have changed it.
func installedTrusted(path string) (version.SemVer, bool) {
	if f, err := CheckServiceBinary(path); err != nil || len(f) > 0 {
		return version.SemVer{}, false
	}
	return version.InstalledVersion(path)
}

// checkEntries fails when anything in dir, at any depth, can be changed by
// anyone but SYSTEM and the administrators or is a link or junction.
func checkEntries(dir string) error {
	return filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		role := RoleFile
		if e.IsDir() {
			role = RoleDir
		}
		return checkStep(ChainStep{Path: p, Role: role})
	})
}

// checkStep fails when principals other than SYSTEM, the administrators and
// TrustedInstaller can replace the service binary through step, or when step
// is a reparse point.
func checkStep(step ChainStep) error {
	sd, err := readSecurity(step.Path)
	if err != nil {
		return err
	}
	if who := UntrustedWriters(sd, step.Role); len(who) > 0 {
		return fmt.Errorf("%s can be changed by %v", step.Path, who)
	}
	return nil
}

// secureServiceDir provides dir as a directory that only SYSTEM and the
// administrators have ever been able to change. A directory this installer
// created (with [serviceDirSDDL] at creation, its identity recorded in the
// registry where only administrators can write) is kept and given that
// security descriptor again. Any other one is not adopted, whatever its
// access list says now: a process that opened it while it was open to others
// may still hold a handle that can change it. It is moved aside and a fresh
// directory is created and recorded.
func secureServiceDir(dir string) error {
	created, err := createProtectedDir(dir)
	if err != nil {
		return err
	}
	if created {
		return recordDir(dir)
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return &os.PathError{Op: "stat", Path: dir, Err: err}
	}
	if attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("%s exists and is not a directory", dir)
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		if id, err := dirIdentity(dir); err == nil && id != "" && id == dirIdentityStore.get() {
			return applySDDL(dir, serviceDirSDDL)
		}
	}
	aside := fmt.Sprintf("%s.untrusted-%d", dir, time.Now().UnixNano())
	if err := os.Rename(dir, aside); err != nil {
		return fmt.Errorf("%s was not created by this installer and others may have changed it, so it is not used; "+
			"stop the service (Stop-Service %s), remove the directory in an elevated prompt (Remove-Item -Recurse %q) and run the command again: %w",
			dir, WindowsServiceName, dir, err)
	}
	if created, err := createProtectedDir(dir); err != nil {
		return err
	} else if !created {
		return fmt.Errorf("%s appeared again while it was being replaced", dir)
	}
	return recordDir(dir)
}

// createProtectedDir creates dir with [serviceDirSDDL] at creation; created
// is false when it exists already.
func createProtectedDir(dir string) (created bool, err error) {
	sd, err := windows.SecurityDescriptorFromString(serviceDirSDDL)
	if err != nil {
		return false, err
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return false, err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	err = windows.CreateDirectory(p, sa)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return false, nil
	}
	return false, &os.PathError{Op: "mkdir", Path: dir, Err: err}
}

func recordDir(dir string) error {
	id, err := dirIdentity(dir)
	if err != nil {
		return err
	}
	if err := dirIdentityStore.set(id); err != nil {
		return fmt.Errorf("cannot record the service directory: %w", err)
	}
	return nil
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

// readSecurity reads the owner and DACL of path, and whether path itself is a
// reparse point (symbolic link, junction, mount point).
func readSecurity(path string) (SecurityInfo, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return SecurityInfo{}, &os.PathError{Op: "get security", Path: path, Err: err}
	}
	info, err := securityInfoOf(sd)
	if err != nil {
		return info, err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return info, err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return info, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	info.Reparse = attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	return info, nil
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
