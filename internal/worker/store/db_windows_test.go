//go:build windows

package store

import (
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// permissiveDir returns a directory whose ACL grants Everyone full control,
// inherited by every file created in it.
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

// TestOpenCreatesProtectedDatabaseInPermissiveDir proves that worker.db is
// created through identity.WritePrivate: the file carries a protected DACL for
// the owner SID and SYSTEM only although its directory grants Everyone (A-11).
func TestOpenCreatesProtectedDatabaseInPermissiveDir(t *testing.T) {
	dir := filepath.Join(permissiveDir(t), "state")
	s := openStore(t, dir)
	if _, err := s.NextSessionNumber(); err != nil {
		t.Fatal(err)
	}
	assertPrivateFiles(t, dir, DBFile, DBFile+"-wal", DBFile+"-shm")
}

// assertPrivateFiles checks identity.CheckPrivate on each named file of dir.
func assertPrivateFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := identity.CheckPrivate(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s is not private: %v", n, err)
		}
	}
}

// TestOpenProtectsExistingPermissiveStateDir opens directly in a state
// directory that already exists and grants Everyone: Open must replace its ACL
// before SQLite creates worker.db, worker.db-wal and worker.db-shm.
func TestOpenProtectsExistingPermissiveStateDir(t *testing.T) {
	dir := permissiveDir(t)
	s := openStore(t, dir)
	if _, err := s.NextSessionNumber(); err != nil {
		t.Fatal(err)
	}
	assertPrivateFiles(t, dir, DBFile, DBFile+"-wal", DBFile+"-shm")
}

// TestMigrationBackupIsPrivateInPermissiveDir proves that the backup that
// precedes a pending migration inherits the private ACL of the state directory.
func TestMigrationBackupIsPrivateInPermissiveDir(t *testing.T) {
	dir := filepath.Join(permissiveDir(t), "state")
	s, err := Open(dir, testMigrations(v1))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	openStore(t, dir, testMigrations(v2))
	backups, err := filepath.Glob(filepath.Join(dir, backupDirName, "worker-*.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}
	if err := identity.CheckPrivate(backups[0]); err != nil {
		t.Fatalf("backup %s is not private: %v", backups[0], err)
	}
}

// The state directory of a service that runs as SYSTEM and serves a pinned user
// grants that user and SYSTEM, inheritable, so that the user keeps access to
// worker.db and its siblings.
func TestApplyPrivateDACLForAPinnedUserGrantsThatUserAndSystem(t *testing.T) {
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := applyPrivateDACLFor(dir, users); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	ctrl, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if ctrl&windows.SE_DACL_PROTECTED == 0 {
		t.Error("the DACL inherits from the parent")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("dacl: %v", err)
	}
	// Windows may keep an inheritable ACE as an effective entry plus an
	// inherit-only one (observed on the runner: two ACEs per trustee), so the
	// test groups ACEs by trustee and requires that each trustee has an ACE
	// that files and directories inherit.
	var sids []*windows.SID
	inheritable := map[string]bool{}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if len(sids) == 0 || !sids[len(sids)-1].Equals(sid) {
			sids = append(sids, sid)
		}
		if ace.Header.AceFlags&(windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE) == windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE {
			inheritable[sid.String()] = true
		}
	}
	for _, sid := range sids {
		if !inheritable[sid.String()] {
			t.Errorf("no ACE of %v is inherited by files and directories", sid)
		}
	}
	if len(sids) != 2 || !sids[0].Equals(users) || !sids[1].Equals(system) {
		t.Fatalf("ACEs = %v, want the pinned user and SYSTEM", sids)
	}
}
