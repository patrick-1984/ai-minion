//go:build windows

package store

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The secret bytes of a backup (the data key, the identity key, the TLS key
// and the database that holds sealed settings) must not become readable
// through the ACL a directory hands down to the files created in it. Files
// are therefore created with a protected DACL that names the owner and SYSTEM
// only, supplied at creation, and the directories this package creates get
// the same DACL (inheritable, so that files another component such as SQLite
// creates in them are private too), also supplied at creation.
// This is the counterpart of the Unix modes 0600 and 0700.

// privateACL builds a DACL that grants full control to the current user and
// to SYSTEM and to nobody else. When inherit is set the entries are inherited
// by the files and directories created below the object.
func privateACL(inherit bool) (*windows.ACL, error) {
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		return nil, err
	}
	owner := u.User.Sid
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	inheritance := uint32(windows.NO_INHERITANCE)
	if inherit {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	// TrusteeValueFromSID stores the SID address as a plain uintptr, which the
	// garbage collector does not follow. Pin both SIDs and keep them reachable
	// until ACLFromEntries has returned, as the x/sys documentation requires.
	var pin runtime.Pinner
	pin.Pin(owner)
	pin.Pin(system)
	defer pin.Unpin()
	defer runtime.KeepAlive(owner)
	defer runtime.KeepAlive(system)
	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(owner),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(system),
			},
		},
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	runtime.KeepAlive(entries)
	return acl, err
}

// privateAttrs holds the SECURITY_ATTRIBUTES that carry a protected DACL for
// the owner and SYSTEM only (SE_DACL_PROTECTED: nothing is inherited from the
// parent directory), with the objects it points to. The caller must call
// keepAlive after the creating system call has returned.
type privateAttrs struct {
	sa  *windows.SecurityAttributes
	sd  *windows.SECURITY_DESCRIPTOR
	acl *windows.ACL
}

// privateAttributes builds privateAttrs; with inherit set the entries are
// inheritable, which is what a directory needs.
func privateAttributes(inherit bool) (*privateAttrs, error) {
	acl, err := privateACL(inherit)
	if err != nil {
		return nil, err
	}
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	if err := sd.SetDACL(acl, true, false); err != nil {
		return nil, err
	}
	if err := sd.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, err
	}
	return &privateAttrs{
		sa: &windows.SecurityAttributes{
			Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
			SecurityDescriptor: sd,
		},
		sd:  sd,
		acl: acl,
	}, nil
}

func (a *privateAttrs) keepAlive() {
	runtime.KeepAlive(a.sd)
	runtime.KeepAlive(a.acl)
	runtime.KeepAlive(a.sa)
}

// createPrivate creates the new file p, which must not exist, for writing.
// The protected DACL (owner and SYSTEM only) is supplied as the
// SECURITY_ATTRIBUTES of CreateFile with CREATE_NEW, so the file never exists
// under the directory's inherited ACL.
func createPrivate(p string) (*os.File, error) {
	attrs, err := privateAttributes(false)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, attrs.sa,
		windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	attrs.keepAlive()
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	return os.NewFile(uintptr(h), p), nil
}

// protectDir replaces the DACL of the directory p with a protected,
// inheritable DACL for the owner and SYSTEM only. It is only for a directory
// that was created through a path this package does not control (the restore
// creates its tls directory through an os.Root) and that holds nothing yet;
// directories this package creates itself use [mkdirPrivate].
func protectDir(p string) error {
	acl, err := privateACL(true)
	if err != nil {
		return &os.PathError{Op: "chmod", Path: p, Err: err}
	}
	err = windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
	runtime.KeepAlive(acl)
	if err != nil {
		return &os.PathError{Op: "chmod", Path: p, Err: err}
	}
	return nil
}

// mkdirPrivate creates the new directory p, which must not exist. The
// protected, inheritable DACL (owner and SYSTEM only) is supplied as the
// SECURITY_ATTRIBUTES of CreateDirectory, so the directory never exists under
// the permissive ACL it would otherwise inherit and nobody can plant a file in
// it first. An existing directory is reported as an exists error and its ACL
// is not touched.
func mkdirPrivate(p string) error {
	attrs, err := privateAttributes(true)
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: p, Err: err}
	}
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: p, Err: err}
	}
	err = windows.CreateDirectory(name, attrs.sa)
	attrs.keepAlive()
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: p, Err: err}
	}
	return nil
}

// mkdirAllPrivate creates the directory p and its missing parents, each one
// private from its creation (see [mkdirPrivate]). A directory that already
// exists, also one that appears concurrently, is left alone.
func mkdirAllPrivate(p string) error {
	var missing []string // outermost last
	for d := filepath.Clean(p); ; {
		if st, err := os.Stat(d); err == nil {
			if !st.IsDir() {
				return &os.PathError{Op: "mkdir", Path: d, Err: windows.ERROR_DIRECTORY}
			}
			break
		}
		missing = append(missing, d)
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := mkdirPrivate(missing[i]); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

// mkdirTempPrivate creates a new directory in dir (pattern as in
// [os.MkdirTemp]) that is private from its creation (see [mkdirPrivate]).
func mkdirTempPrivate(dir, pattern string) (string, error) {
	prefix, suffix, _ := strings.Cut(pattern, "*")
	for range 10000 {
		var r [8]byte
		if _, err := rand.Read(r[:]); err != nil {
			return "", err
		}
		name := filepath.Join(dir, prefix+hex.EncodeToString(r[:])+suffix)
		if err := mkdirPrivate(name); err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		return name, nil
	}
	return "", &os.PathError{Op: "mkdirtemp", Path: filepath.Join(dir, pattern), Err: os.ErrExist}
}
