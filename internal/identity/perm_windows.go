//go:build windows

package identity

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// accessMask is every right that CheckPrivate treats as access: any read,
// write, append, execute, delete, WRITE_DAC, WRITE_OWNER or generic right.
// A principal other than the user, the file owner and SYSTEM must hold none
// of them (the Windows counterpart of the Unix group and other bits 0077).
// READ_CONTROL and SYNCHRONIZE alone do not give access to the content.
const accessMask = windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
	windows.FILE_READ_EA | windows.FILE_WRITE_EA | windows.FILE_EXECUTE |
	windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES |
	windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
	windows.GENERIC_READ | windows.GENERIC_WRITE | windows.GENERIC_EXECUTE | windows.GENERIC_ALL |
	windows.MAXIMUM_ALLOWED

// currentUserSID returns the SID of the user running the process.
func currentUserSID() (*windows.SID, error) {
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid, nil
}

func systemSID() (*windows.SID, error) {
	return windows.CreateWellKnownSid(windows.WinLocalSystemSid)
}

// PrivateUser returns the SID of the user that the private files of this
// process belong to: the process user, or, in a process that runs as SYSTEM
// with a valid [PipeUserSIDEnv], the pinned user (see [PrivateUserSID]).
func PrivateUser() (*windows.SID, error) {
	self, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	chosen := PrivateUserSID(self.String(), os.Getenv(PipeUserSIDEnv))
	if chosen == self.String() {
		return self, nil
	}
	return windows.StringToSid(chosen)
}

// privateACL builds a DACL that grants full control to the private user (see
// [PrivateUser]) and to SYSTEM and to nobody else.
func privateACL() (*windows.ACL, error) {
	owner, err := PrivateUser()
	if err != nil {
		return nil, err
	}
	return privateACLFor(owner)
}

// privateACLFor builds the DACL of [privateACL] for the given user.
func privateACLFor(owner *windows.SID) (*windows.ACL, error) {
	system, err := systemSID()
	if err != nil {
		return nil, err
	}
	// TrusteeValueFromSID stores the SID address as a plain uintptr, which the
	// garbage collector does not follow. Pin both SIDs and keep them reachable
	// until SetEntriesInAcl has returned, as the x/sys documentation requires.
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
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(owner),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
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

// createPrivateTemp creates a new temporary file in dir (pattern as in
// os.CreateTemp) with a protected DACL for the owner SID and SYSTEM only,
// supplied as the SECURITY_ATTRIBUTES of CreateFile with CREATE_NEW. The file
// therefore never exists under the directory's inherited ACL, and no other
// principal can open a handle in a window before the DACL is applied.
func createPrivateTemp(dir, pattern string) (*os.File, error) {
	acl, err := privateACL()
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
	// SE_DACL_PROTECTED: do not inherit ACEs from the directory.
	if err := sd.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	prefix, suffix, _ := strings.Cut(pattern, "*")
	for range 10000 {
		var r [8]byte
		if _, err := rand.Read(r[:]); err != nil {
			return nil, err
		}
		name := filepath.Join(dir, prefix+hex.EncodeToString(r[:])+suffix)
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return nil, err
		}
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, sa,
			windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		// acl and sd must stay reachable until CreateFile has returned.
		runtime.KeepAlive(sd)
		runtime.KeepAlive(acl)
		if err == windows.ERROR_FILE_EXISTS || err == windows.ERROR_ALREADY_EXISTS {
			continue
		}
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: name, Err: err}
		}
		return os.NewFile(uintptr(h), name), nil
	}
	return nil, &os.PathError{Op: "createtemp", Path: filepath.Join(dir, pattern), Err: os.ErrExist}
}

// syncDir is a no-op on Windows: directories cannot be opened for sync.
func syncDir(string) {}

// CheckPrivate returns an error when path is not a regular file or when its
// DACL grants any access (read, write, append, execute, delete, WRITE_DAC,
// WRITE_OWNER or a generic right, see accessMask) to a principal other than
// the user, the file's owner and SYSTEM. A missing DACL (everyone has full
// access) is refused. It fails closed: an ACE type other than plain allow or
// deny (callback, object, conditional and similar) is refused because it may
// grant access this check cannot evaluate; only ACEs flagged INHERIT_ONLY_ACE,
// which do not apply to the file itself, are skipped.
func CheckPrivate(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return newErr(kindInvalidArgument, fmt.Sprintf("%s is not a regular file", path))
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return checkSecurity(path, sd, false)
}

// OpenPrivate opens the existing secret file path for reading and checks the
// file it opened: it must be a regular file, not a reparse point (a symbolic
// link or junction is refused even when its target is private), and its DACL
// must grant access to nobody but the user, the file's owner and SYSTEM (see
// [CheckPrivate]); unlike CheckPrivate it also requires that the owner is one of
// the identities of this process and that the DACL is protected (see
// [CheckPrivateFile]). The path is opened once, without following a reparse point,
// and the DACL is read from that handle, so there is no window between a check
// by path and the read in which the file can be swapped. Nothing is changed on
// the file. A missing file is an error that matches os.ErrNotExist. The caller
// closes the handle.
func OpenPrivate(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	if err := CheckPrivateFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// CheckPrivateFile is the check of [OpenPrivate] on an open file. The owner is
// validated first, because an owner can always change the DACL: it must be the
// user that runs this process, SYSTEM, or the default owner of the process token
// (BUILTIN\Administrators for an elevated administrator), never another
// account whose DACL merely grants itself control. Then the DACL must be
// protected (SE_DACL_PROTECTED: nothing is inherited from the directory, so a
// later change of the directory's ACL cannot expose the file), and then no other
// principal may have access (see [CheckPrivate]). It never repairs the file.
func CheckPrivateFile(f *os.File) error {
	h := windows.Handle(f.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return newErr(kindInvalidArgument, fmt.Sprintf("%s is a symbolic link or junction; it must be the file itself", f.Name()))
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return newErr(kindInvalidArgument, fmt.Sprintf("%s is not a regular file", f.Name()))
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return checkSecurity(f.Name(), sd, true)
}

// CheckPrivateDir has nothing to add on Windows: the directories a Windows Hub
// creates carry a protected owner-and-SYSTEM-only DACL from their creation, and
// access to what lies in an existing directory is decided by the DACL of the
// file itself, which [CheckPrivateFile] checks. It only reports a missing
// directory.
func CheckPrivateDir(dir string) error {
	_, err := os.Stat(dir)
	return err
}

// tokenDefaultOwner returns the owner that objects created by this process get
// by default (TokenOwner): the user, or BUILTIN\Administrators for an elevated
// administrator.
func tokenDefaultOwner() (*windows.SID, error) {
	tok := windows.GetCurrentProcessToken()
	var n uint32
	_ = windows.GetTokenInformation(tok, windows.TokenOwner, nil, 0, &n)
	if n == 0 {
		return nil, fmt.Errorf("cannot read the default owner of the process token")
	}
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(tok, windows.TokenOwner, &buf[0], n, &n); err != nil {
		return nil, err
	}
	owner := *(**windows.SID)(unsafe.Pointer(&buf[0]))
	return owner.Copy()
}

// ownerPermitted reports whether owner may own a secret file of this process:
// the user, SYSTEM or the default owner of the process token.
func ownerPermitted(owner, user, system *windows.SID) bool {
	if owner == nil {
		return false
	}
	if owner.Equals(user) || owner.Equals(system) {
		return true
	}
	if def, err := tokenDefaultOwner(); err == nil && owner.Equals(def) {
		return true
	}
	return false
}

// checkSecurity is the DACL test of CheckPrivate on a security descriptor. With
// strict (an opened secret file, see CheckPrivateFile) the owner is validated
// first and the DACL must be protected; without it the owner of the file is
// trusted as before.
func checkSecurity(path string, sd *windows.SECURITY_DESCRIPTOR, strict bool) error {
	user, err := PrivateUser()
	if err != nil {
		return err
	}
	system, err := systemSID()
	if err != nil {
		return err
	}
	fileOwner, _, _ := sd.Owner()
	if strict {
		if !ownerPermitted(fileOwner, user, system) {
			who := "no owner"
			if fileOwner != nil {
				who = fileOwner.String()
			}
			return newErr(kindInvalidArgument, fmt.Sprintf("%s is owned by %s, not by the user that runs this program or SYSTEM; its owner can change who may read it", path, who))
		}
		ctrl, _, err := sd.Control()
		if err != nil {
			return err
		}
		if ctrl&windows.SE_DACL_PROTECTED == 0 {
			return newErr(kindInvalidArgument, fmt.Sprintf("%s does not have a protected DACL: it inherits access from its directory, so a change of that directory's ACL can expose it", path))
		}
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return newErr(kindInvalidArgument, fmt.Sprintf("%s has no DACL; everyone has access", path))
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return newErr(kindInvalidArgument, fmt.Sprintf("%s has an ACE of unsupported type %#x; refusing", path, ace.Header.AceType))
		}
		if ace.Mask&accessMask == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(user) || sid.Equals(system) || (fileOwner != nil && sid.Equals(fileOwner)) {
			continue
		}
		return newErr(kindInvalidArgument, fmt.Sprintf("%s grants access to %s; only the owner and SYSTEM may access it", path, sid.String()))
	}
	return nil
}
