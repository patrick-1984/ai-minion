//go:build windows

package version

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileDeleteChild is FILE_DELETE_CHILD: the right to delete or rename an entry
// of a directory whatever the entry's own DACL says. x/sys does not name it.
const fileDeleteChild = 0x40

// Rights that let a principal change what a directory or file holds or who may
// access it. A trusted-directory check refuses an untrusted principal that is
// granted any of them. Reading is not refused: the checked content is public.
const (
	// dirWriteMask applies to the directory itself: add a file or a
	// subdirectory, remove or rename an entry, change attributes or the DACL.
	dirWriteMask = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA |
		windows.FILE_WRITE_ATTRIBUTES | fileDeleteChild | windows.DELETE | windows.WRITE_DAC |
		windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL | windows.MAXIMUM_ALLOWED
	// ancestorMask applies to every directory above it: adding an entry beside
	// the checked directory is harmless, removing or renaming it, or taking
	// over the ancestor, is not.
	ancestorMask = fileDeleteChild | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
		windows.GENERIC_ALL | windows.MAXIMUM_ALLOWED
	// fileWriteMask applies to a file opened by openTrusted.
	fileWriteMask = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA |
		windows.FILE_WRITE_ATTRIBUTES | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
		windows.GENERIC_WRITE | windows.GENERIC_ALL | windows.MAXIMUM_ALLOWED
)

// trustedInstallerSID is NT SERVICE\TrustedInstaller, the owner of the Windows
// system directories.
const trustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

// trustedPrincipals returns the principals that may own and write what a
// trusted directory holds: the user that runs this program,
// SYSTEM, BUILTIN\Administrators, TrustedInstaller, CREATOR OWNER (it resolves
// to the owner, which is checked) and the default owner of the process token.
func trustedPrincipals() ([]*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	out := []*windows.SID{user.User.Sid}
	for _, w := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid, windows.WinCreatorOwnerSid} {
		s, err := windows.CreateWellKnownSid(w)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if ti, err := windows.StringToSid(trustedInstallerSID); err == nil {
		out = append(out, ti)
	}
	if def, err := tokenDefaultOwner(); err == nil {
		out = append(out, def)
	}
	return out, nil
}

func sidIn(sid *windows.SID, set []*windows.SID) bool {
	if sid == nil {
		return false
	}
	for _, s := range set {
		if sid.Equals(s) {
			return true
		}
	}
	return false
}

// untrustedAccess checks a security descriptor against trusted: its owner must
// be trusted (an owner can always change the DACL), the DACL must exist, and no
// allow ACE that applies to the object itself may grant a right of mask to a
// principal outside trusted. It fails closed: an ACE type other than plain
// allow or deny may grant what this check cannot evaluate and is refused. Deny
// ACEs are ignored, which only makes the check stricter.
//
// When leaf is set, path is a directory this program is about to create
// entries in (the cache root, the directory of the installed executable) and
// the check also covers what those entries will inherit: an inherit-only ACE
// (one that does not apply to the directory itself but is copied onto every
// new subdirectory or file) that grants a right of mask to a principal outside
// trusted would give that principal write access to the staging directory, the
// downloaded archive, the extracted executable or the temporary copy that is
// renamed over the installed one, so it is refused too. Directories above a
// leaf are not created into and an inherit-only ACE on them cannot reach what
// was already checked (inheritance happens when an object is created), so for
// them it stays ignored.
func untrustedAccess(path string, sd *windows.SECURITY_DESCRIPTOR, mask uint32, leaf bool, trusted []*windows.SID) error {
	owner, _, _ := sd.Owner()
	if !sidIn(owner, trusted) {
		who := "no owner"
		if owner != nil {
			who = owner.String()
		}
		return errUntrusted(fmt.Sprintf("%s is owned by %s, which is neither the user that runs this program nor an administrative account, so its owner can replace what is below it", path, who))
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return errUntrusted(fmt.Sprintf("%s has no DACL; everyone has access", path))
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		inheritOnly := ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0
		inherits := ace.Header.AceFlags&(windows.CONTAINER_INHERIT_ACE|windows.OBJECT_INHERIT_ACE) != 0
		if inheritOnly && (!leaf || !inherits) {
			continue
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return errUntrusted(fmt.Sprintf("%s has an ACE of unsupported type %#x; refusing", path, ace.Header.AceType))
		}
		aceMask := mask
		if inheritOnly && ace.Header.AceFlags&windows.CONTAINER_INHERIT_ACE == 0 {
			// Only files inherit it (a new subdirectory does not), so the
			// rights of a directory that have no meaning for a file do not count.
			aceMask = fileWriteMask
		}
		if uint32(ace.Mask)&aceMask == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sidIn(sid, trusted) {
			continue
		}
		if inheritOnly {
			return errUntrusted(fmt.Sprintf("%s grants write access to %s on everything created below it (an inheritable ACE), so that principal could change the staged or installed files", path, sid.String()))
		}
		return errUntrusted(fmt.Sprintf("%s grants write access to %s, so that principal can replace what is below it", path, sid.String()))
	}
	return nil
}

// isReparse reports whether path is a reparse point (symbolic link, junction).
func isReparse(path string) (bool, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return false, err
	}
	return attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}

// tokenDefaultOwner returns the owner that objects created by this process get
// by default (TokenOwner): the user, or BUILTIN\\Administrators for an elevated
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

// checkTrustedDir returns an error when a principal other than the user that
// runs this program or an administrative account could replace, add or remove
// entries in dir, could get write access to what is created in it through an
// inheritable ACE, or could replace a directory on the way to it. dir itself must not
// be a reparse point (symbolic link or junction) and must be owned by a trusted
// principal with no write access granted to anyone else; every directory above
// it, along the path as given and along the path with links resolved, must be
// owned by a trusted principal and must not grant delete, rename or
// ownership rights to anyone else (adding a sibling is harmless, and so is an
// inherit-only ACE, which only reaches objects created later). It fails
// closed on an ACE it cannot evaluate. A missing directory is an error that
// matches os.ErrNotExist.
func checkTrustedDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errUntrusted(fmt.Sprintf("%s is not a directory", abs))
	}
	if re, err := isReparse(abs); err != nil {
		return err
	} else if re {
		return errUntrusted(fmt.Sprintf("%s is a symbolic link or junction; it must be the directory itself", abs))
	}
	trusted, err := trustedPrincipals()
	if err != nil {
		return err
	}
	chains := []string{abs}
	if !strings.EqualFold(real, abs) {
		chains = append(chains, real)
	}
	for _, p := range chains {
		if err := checkChain(p, trusted); err != nil {
			return err
		}
	}
	return nil
}

// checkChain checks every directory from the volume root down to p.
func checkChain(p string, trusted []*windows.SID) error {
	vol := filepath.VolumeName(p)
	cur := vol + `\`
	rest := strings.TrimLeft(p[len(vol):], `\/`)
	steps := []string{cur}
	if rest != "" {
		for _, comp := range strings.Split(rest, `\`) {
			if comp == "" {
				continue
			}
			cur = filepath.Join(cur, comp)
			steps = append(steps, cur)
		}
	}
	for i, s := range steps {
		mask := uint32(ancestorMask)
		leaf := i == len(steps)-1
		if leaf {
			mask = dirWriteMask
		}
		sd, err := windows.GetNamedSecurityInfo(s, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			return err
		}
		if err := untrustedAccess(s, sd, mask, leaf, trusted); err != nil {
			return err
		}
	}
	return nil
}

// openTrusted opens the existing regular file path for reading and checks the
// file it opened: it is not a reparse point (symbolic link or junction), it is
// owned by a trusted principal and its DACL grants write access to nobody else.
// The path is opened once, without following a reparse point, and the checks are
// made on the handle, so the bytes read are the bytes that were checked. It is
// meant for files below a directory that [checkTrustedDir] accepted; the file
// may be readable by others (it is not secret). The caller closes the handle.
func openTrusted(path string) (*os.File, error) {
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
	if err := checkTrustedHandle(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func checkTrustedHandle(f *os.File) error {
	h := windows.Handle(f.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errUntrusted(fmt.Sprintf("%s is a symbolic link or junction; it must be the file itself", f.Name()))
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return errUntrusted(fmt.Sprintf("%s is not a regular file", f.Name()))
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	trusted, err := trustedPrincipals()
	if err != nil {
		return err
	}
	return untrustedAccess(f.Name(), sd, fileWriteMask, false, trusted)
}

// checkInstallDir is checkTrustedDir for the directory of the executable that
// is replaced: the temporary copy and the renamed old executable are created
// in it, so it and every directory above it must be changeable by trusted
// principals only, and must not hand write access to anyone else on what is
// created in it.
func checkInstallDir(dir string) error { return checkTrustedDir(dir) }

// checkNewFile checks a file this program has just created (it inherited its
// DACL and owner from the directory it is in): trusted owner, DACL without
// write access for anyone else. It runs on the open handle, before any bytes
// are written.
func checkNewFile(f *os.File) error { return checkTrustedHandle(f) }
