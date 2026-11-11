//go:build windows

package identity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// permissiveDir returns a directory whose ACL grants Everyone full control, inherited
// by every file created in it.
func permissiveDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	return dir
}

// aceSIDs returns the SIDs of the ACEs of the DACL of path and whether the
// DACL is protected (does not inherit). It fails the test when an ACE is not a
// plain allow ACE.
func aceSIDs(t *testing.T, path string) ([]*windows.SID, bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	ctrl, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("dacl: %v", err)
	}
	var sids []*windows.SID
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("ACE %d has type %d, want plain allow", i, ace.Header.AceType)
		}
		sids = append(sids, (*windows.SID)(unsafe.Pointer(&ace.SidStart)))
	}
	return sids, ctrl&windows.SE_DACL_PROTECTED != 0
}

// assertOwnerSystemOnly fails unless path has a protected DACL with exactly
// two ACEs: the current user and SYSTEM.
func assertOwnerSystemOnly(t *testing.T, path string) {
	t.Helper()
	sids, protected := aceSIDs(t, path)
	if !protected {
		t.Error("DACL inherits from the directory (SE_DACL_PROTECTED not set)")
	}
	user, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	system, err := systemSID()
	if err != nil {
		t.Fatal(err)
	}
	if len(sids) != 2 {
		t.Fatalf("want exactly 2 ACEs, got %d", len(sids))
	}
	var hasUser, hasSystem bool
	for _, s := range sids {
		hasUser = hasUser || s.Equals(user)
		hasSystem = hasSystem || s.Equals(system)
	}
	if !hasUser || !hasSystem {
		t.Errorf("ACEs are not exactly owner and SYSTEM: user=%v system=%v", hasUser, hasSystem)
	}
}

func trustee(sid *windows.SID, typ windows.TRUSTEE_TYPE) windows.TRUSTEE {
	return windows.TRUSTEE{
		TrusteeForm:  windows.TRUSTEE_IS_SID,
		TrusteeType:  typ,
		TrusteeValue: windows.TrusteeValueFromSID(sid),
	}
}

// setProtectedDACL replaces the DACL of path with a protected DACL made of the
// given entries; nil entries create a NULL DACL (everyone has full access).
func setProtectedDACL(t *testing.T, path string, entries []windows.EXPLICIT_ACCESS) {
	t.Helper()
	var acl *windows.ACL
	if entries != nil {
		var err error
		if acl, err = windows.ACLFromEntries(entries, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

func ownerSystemEntries(t *testing.T) []windows.EXPLICIT_ACCESS {
	t.Helper()
	user, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	system, err := systemSID()
	if err != nil {
		t.Fatal(err)
	}
	return []windows.EXPLICIT_ACCESS{
		{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.GRANT_ACCESS,
			Inheritance: windows.NO_INHERITANCE, Trustee: trustee(user, windows.TRUSTEE_IS_USER)},
		{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.GRANT_ACCESS,
			Inheritance: windows.NO_INHERITANCE, Trustee: trustee(system, windows.TRUSTEE_IS_WELL_KNOWN_GROUP)},
	}
}

func everyoneEntry(t *testing.T, access uint32) windows.EXPLICIT_ACCESS {
	t.Helper()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	return windows.EXPLICIT_ACCESS{AccessPermissions: windows.ACCESS_MASK(access), AccessMode: windows.GRANT_ACCESS,
		Inheritance: windows.NO_INHERITANCE, Trustee: trustee(everyone, windows.TRUSTEE_IS_WELL_KNOWN_GROUP)}
}

func TestSaveKeyProtectedDACL(t *testing.T) {
	dir := permissiveDir(t)
	path := filepath.Join(dir, "id.key")
	if err := SaveKey(path, mustKey(t)); err != nil {
		t.Fatal(err)
	}
	assertOwnerSystemOnly(t, path)
	if _, err := LoadKey(path); err != nil {
		t.Fatalf("load own key: %v", err)
	}
}

// Story 2.3 relies on WritePrivate: it must give the same protection as
// SaveKey.
func TestWritePrivateProtectedDACL(t *testing.T) {
	path := filepath.Join(permissiveDir(t), "state.db")
	if err := WritePrivate(path, []byte("x")); err != nil {
		t.Fatal(err)
	}
	assertOwnerSystemOnly(t, path)
	if err := CheckPrivate(path); err != nil {
		t.Fatal(err)
	}
	// Replacing an existing file keeps the protection.
	if err := WritePrivate(path, []byte("y")); err != nil {
		t.Fatal(err)
	}
	assertOwnerSystemOnly(t, path)
}

// The temporary file is private at creation, before any byte is written, even
// in a directory whose inherited ACL grants Everyone full control.
func TestCreatePrivateTempProtectedAtCreation(t *testing.T) {
	dir := permissiveDir(t)
	f, err := createPrivateTemp(dir, ".state.tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	assertOwnerSystemOnly(t, f.Name())
	if _, err := f.Write([]byte("secret")); err != nil {
		t.Fatal(err)
	}
	assertOwnerSystemOnly(t, f.Name())
	g, err := createPrivateTemp(dir, ".state.tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	if f.Name() == g.Name() {
		t.Fatal("temporary names collide")
	}
}

func TestLoadKeyRefusesOtherPrincipals(t *testing.T) {
	cases := []struct {
		name    string
		entries func(t *testing.T) []windows.EXPLICIT_ACCESS // nil result: NULL DACL
	}{
		{"everyone read", func(t *testing.T) []windows.EXPLICIT_ACCESS {
			return append(ownerSystemEntries(t), everyoneEntry(t, windows.FILE_GENERIC_READ))
		}},
		{"everyone write only", func(t *testing.T) []windows.EXPLICIT_ACCESS {
			return append(ownerSystemEntries(t), everyoneEntry(t, windows.FILE_WRITE_DATA))
		}},
		{"everyone append only", func(t *testing.T) []windows.EXPLICIT_ACCESS {
			return append(ownerSystemEntries(t), everyoneEntry(t, windows.FILE_APPEND_DATA))
		}},
		{"everyone delete", func(t *testing.T) []windows.EXPLICIT_ACCESS {
			return append(ownerSystemEntries(t), everyoneEntry(t, windows.DELETE))
		}},
		{"everyone write dac", func(t *testing.T) []windows.EXPLICIT_ACCESS {
			return append(ownerSystemEntries(t), everyoneEntry(t, windows.WRITE_DAC))
		}},
		{"null dacl", func(*testing.T) []windows.EXPLICIT_ACCESS { return nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(permissiveDir(t), "id.key")
			if err := SaveKey(path, mustKey(t)); err != nil {
				t.Fatal(err)
			}
			setProtectedDACL(t, path, tc.entries(t))
			if _, err := LoadKey(path); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// Control: an explicit protected DACL of owner and SYSTEM only is accepted.
func TestLoadKeyAcceptsOwnerAndSystem(t *testing.T) {
	path := filepath.Join(permissiveDir(t), "id.key")
	if err := SaveKey(path, mustKey(t)); err != nil {
		t.Fatal(err)
	}
	setProtectedDACL(t, path, ownerSystemEntries(t))
	if _, err := LoadKey(path); err != nil {
		t.Fatal(err)
	}
}

// A conditional allow ACE for Everyone must not slip through: CheckPrivate
// refuses any ACE type it cannot evaluate.
func TestLoadKeyRefusesConditionalAllowACE(t *testing.T) {
	path := filepath.Join(permissiveDir(t), "id.key")
	if err := SaveKey(path, mustKey(t)); err != nil {
		t.Fatal(err)
	}
	user, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	sddl := "D:P(A;;FA;;;" + user.String() + ")(A;;FA;;;SY)(XA;;FR;;;WD;(@User.Title==\"PM\"))"
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Skipf("cannot build a conditional ACE on this host: %v", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Skipf("cannot apply a conditional ACE on this host: %v", err)
	}
	if _, err := LoadKey(path); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
}

// aclSIDs returns the SIDs of the ACEs of acl.
func aclSIDs(t *testing.T, acl *windows.ACL) []*windows.SID {
	t.Helper()
	var sids []*windows.SID
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			t.Fatal(err)
		}
		sids = append(sids, (*windows.SID)(unsafe.Pointer(&ace.SidStart)))
	}
	return sids
}

// The private ACL of a service that serves a pinned user grants that user and
// SYSTEM, not SYSTEM twice (the installing user keeps access to their state).
func TestPrivateACLForAPinnedUserGrantsThatUserAndSystem(t *testing.T) {
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatal(err)
	}
	acl, err := privateACLFor(users)
	if err != nil {
		t.Fatal(err)
	}
	sids := aclSIDs(t, acl)
	system, err := systemSID()
	if err != nil {
		t.Fatal(err)
	}
	if len(sids) != 2 || !sids[0].Equals(users) || !sids[1].Equals(system) {
		t.Fatalf("ACEs = %v, want the pinned user and SYSTEM", sids)
	}
}

// A process that does not run as SYSTEM ignores the pin: the private user is
// the process user.
func TestPrivateUserIgnoresThePinOutsideSystem(t *testing.T) {
	self, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if self.String() == SIDSystem {
		t.Skip("the test runs as SYSTEM")
	}
	t.Setenv(PipeUserSIDEnv, pinnedSID)
	got, err := PrivateUser()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equals(self) {
		t.Fatalf("PrivateUser = %s, want the process user %s", got, self)
	}
}

// OpenPrivate accepts a file made by WritePrivate (protected owner-and-SYSTEM
// DACL), refuses one that inherited a permissive DACL, and never follows a
// symbolic link; the DACL it checks is the one of the opened handle.
func TestOpenPrivateChecksTheOpenedHandle(t *testing.T) {
	private := filepath.Join(t.TempDir(), "secret")
	if err := WritePrivate(private, []byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	f, err := OpenPrivate(private)
	if err != nil {
		t.Fatalf("a private file was refused: %v", err)
	}
	b := make([]byte, 16)
	if _, err := f.Read(b); err != nil || string(b) != "0123456789abcdef" {
		t.Errorf("read %q: %v", b, err)
	}
	_ = f.Close()

	exposed := filepath.Join(permissiveDir(t), "secret")
	if err := os.WriteFile(exposed, []byte("0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenPrivate(exposed); err == nil {
		_ = f.Close()
		t.Error("a file that inherited an Everyone ACL was accepted")
	} else if !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("err = %v, want ErrInvalidArgument", err)
	}

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(private, link); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	if f, err := OpenPrivate(link); err == nil {
		_ = f.Close()
		t.Error("a symbolic link was followed")
	}
	if _, err := OpenPrivate(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing file must stay os.ErrNotExist, got %v", err)
	}
}

// securityDescriptor builds an in-memory descriptor: no file and no privilege is
// needed to describe "owned by another account, readable by the Hub user".
func securityDescriptor(t *testing.T, owner *windows.SID, entries []windows.EXPLICIT_ACCESS, protected bool) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	if err := sd.SetOwner(owner, false); err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sd.SetDACL(acl, true, false); err != nil {
		t.Fatal(err)
	}
	if protected {
		if err := sd.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
			t.Fatal(err)
		}
	}
	return sd
}

// An owner that is not the user, SYSTEM or the token's default owner is refused
// before the DACL is trusted, even when the DACL grants the Hub user read access
// and only the owner full control: that owner can rewrite the DACL and the file.
func TestStrictCheckValidatesTheOwnerBeforeTheDACL(t *testing.T) {
	user, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	system, err := systemSID()
	if err != nil {
		t.Fatal(err)
	}
	for name, sid := range map[string]windows.WELL_KNOWN_SID_TYPE{
		"Everyone":       windows.WinWorldSid,
		"BUILTIN\\Users": windows.WinBuiltinUsersSid,
		"Guests":         windows.WinBuiltinGuestsSid,
	} {
		foreign, err := windows.CreateWellKnownSid(sid)
		if err != nil {
			t.Fatal(err)
		}
		if foreign.Equals(user) {
			continue
		}
		// The foreign owner has full control; the Hub user can read; SYSTEM has full control.
		entries := []windows.EXPLICIT_ACCESS{
			{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.GRANT_ACCESS, Trustee: trustee(foreign, windows.TRUSTEE_IS_WELL_KNOWN_GROUP)},
			{AccessPermissions: windows.GENERIC_READ, AccessMode: windows.GRANT_ACCESS, Trustee: trustee(user, windows.TRUSTEE_IS_USER)},
			{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.GRANT_ACCESS, Trustee: trustee(system, windows.TRUSTEE_IS_WELL_KNOWN_GROUP)},
		}
		sd := securityDescriptor(t, foreign, entries, true)
		// The lenient check, used for other files, exempts the owner: this is the bypass.
		if err := checkSecurity("data.key", sd, false); err != nil {
			t.Fatalf("%s: the lenient check was expected to accept this descriptor: %v", name, err)
		}
		err = checkSecurity("data.key", sd, true)
		if err == nil {
			t.Errorf("%s: a key owned by another account was accepted", name)
		} else if !strings.Contains(err.Error(), "owned by") {
			t.Errorf("%s: the refusal does not name the owner: %v", name, err)
		}
	}
}

// An existing key whose ACL is restrictive today but inherits from its directory
// is refused: the directory's ACL can change later.
func TestStrictCheckRequiresAProtectedDACL(t *testing.T) {
	user, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	sd := securityDescriptor(t, user, ownerSystemEntries(t), false)
	err = checkSecurity("data.key", sd, true)
	if err == nil || !strings.Contains(err.Error(), "protected DACL") {
		t.Errorf("an unprotected DACL was accepted or refused for another reason: %v", err)
	}
	sd = securityDescriptor(t, user, ownerSystemEntries(t), true)
	if err := checkSecurity("data.key", sd, true); err != nil {
		t.Errorf("the user's own protected owner-and-SYSTEM descriptor was refused: %v", err)
	}
}

// The same on a real file: made private, then given an unprotected ACL that names
// only the user and SYSTEM. The opened handle is checked, nothing is repaired.
func TestOpenPrivateRefusesAnExistingKeyWithAnUnprotectedDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.key")
	if err := WritePrivate(path, []byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries(ownerSystemEntries(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	f, err := OpenPrivate(path)
	if err == nil {
		_ = f.Close()
		t.Fatal("an existing key with an unprotected DACL was accepted")
	}
	if !strings.Contains(err.Error(), "protected DACL") {
		t.Errorf("err = %v", err)
	}
	// It was not repaired behind the operator's back.
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if ctrl, _, _ := sd.Control(); ctrl&windows.SE_DACL_PROTECTED != 0 {
		t.Error("the refused file was made protected by the check")
	}
}

// LoadKey opens the key with OpenPrivate: a symbolic link is refused even when
// its target is a private key, and a key that inherited a permissive DACL is
// refused (the owner and DACL checks are those of OpenPrivate).
func TestLoadKeyUsesOpenPrivateChecks(t *testing.T) {
	private := filepath.Join(t.TempDir(), "id.key")
	if err := SaveKey(private, mustKey(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(private); err != nil {
		t.Fatalf("a private key was refused: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(private, link); err != nil {
		t.Logf("cannot create a symbolic link here: %v", err)
	} else if _, err := LoadKey(link); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("a symbolic link was followed: %v", err)
	}
	exposed := filepath.Join(permissiveDir(t), "id.key")
	if err := os.WriteFile(exposed, []byte(keyFileHeader+EncodeB64(make([]byte, 32))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(exposed); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("a key with an inherited permissive DACL was accepted: %v", err)
	}
	if _, err := LoadKey(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing key must stay os.ErrNotExist, got %v", err)
	}
}
