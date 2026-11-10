//go:build windows

package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// permissiveDir returns a directory whose ACL grants Everyone full control,
// inherited by every file and directory created in it.
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

// assertOwnerSystemOnly fails unless path has plain allow ACEs for exactly the
// current user and SYSTEM. Objects created with an explicit DACL (the files of
// createPrivate, the directories of mkdirPrivate) must also carry the protected
// flag (requireProtected). A file that another component creates in a private
// directory gets its ACEs by inheritance and has no protected flag, so there
// only the ACEs are checked.
func assertOwnerSystemOnly(t *testing.T, path string, requireProtected bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	ctrl, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if requireProtected && ctrl&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("%s: the DACL inherits from the directory (SE_DACL_PROTECTED not set)", path)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("%s: dacl: %v", path, err)
	}
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	var hasUser, hasSystem bool
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("%s: ACE %d has type %d, want plain allow", path, i, ace.Header.AceType)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.Equals(u.User.Sid):
			hasUser = true
		case sid.Equals(system):
			hasSystem = true
		default:
			t.Errorf("%s grants access to %s", path, sid.String())
		}
	}
	if !hasUser || !hasSystem {
		t.Errorf("%s: ACEs are not owner and SYSTEM: user=%v system=%v", path, hasUser, hasSystem)
	}
}

func TestCreatePrivateIsProtectedAtCreation(t *testing.T) {
	p := filepath.Join(permissiveDir(t), "secret")
	f, err := createPrivate(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	assertOwnerSystemOnly(t, p, true)
	if _, err := f.Write([]byte("secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := createPrivate(p); !os.IsExist(err) {
		t.Fatalf("second create: err = %v, want exists", err)
	}
}

func TestMkdirPrivateDirectoriesAreProtected(t *testing.T) {
	parent := permissiveDir(t)
	nested := filepath.Join(parent, "a", "b")
	if err := mkdirAllPrivate(nested); err != nil {
		t.Fatal(err)
	}
	assertOwnerSystemOnly(t, filepath.Join(parent, "a"), true)
	assertOwnerSystemOnly(t, nested, true)
	// A file another component creates in it is private as well.
	if err := os.WriteFile(filepath.Join(nested, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp, err := mkdirTempPrivate(parent, ".stage-*")
	if err != nil {
		t.Fatal(err)
	}
	assertOwnerSystemOnly(t, tmp, true)
	if err := os.WriteFile(filepath.Join(tmp, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertOwnerSystemOnly(t, filepath.Join(tmp, "x"), false)
}

// A backup written into a permissive directory, and the files a restore
// creates in a permissive parent, are readable by the owner and SYSTEM only.
func TestBackupAndRestoreFilesAreProtectedInPermissiveDirectories(t *testing.T) {
	ctx := context.Background()
	src := newHubDir(t)
	parent := permissiveDir(t)
	archive := filepath.Join(parent, "out", "hub.tar.gz")
	if _, err := Backup(ctx, src, archive, "dev"); err != nil {
		t.Fatal(err)
	}
	assertOwnerSystemOnly(t, archive, true)
	assertOwnerSystemOnly(t, filepath.Dir(archive), true)

	target := filepath.Join(permissiveDir(t), "hub")
	if _, err := Restore(ctx, archive, target, false); err != nil {
		t.Fatal(err)
	}
	assertOwnerSystemOnly(t, target, true)
	assertOwnerSystemOnly(t, filepath.Join(target, "tls"), true)
	for _, name := range []string{DataKeyFile, IdentityKeyFile, DBFile, "tls/cert.pem", "tls/key.pem"} {
		assertOwnerSystemOnly(t, filepath.Join(target, filepath.FromSlash(name)), false)
	}

	// Restoring over an existing directory keeps its files private too.
	if _, err := Restore(ctx, archive, target, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{DataKeyFile, IdentityKeyFile, DBFile, "tls/key.pem"} {
		assertOwnerSystemOnly(t, filepath.Join(target, filepath.FromSlash(name)), false)
	}
}
