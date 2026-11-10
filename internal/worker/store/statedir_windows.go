//go:build windows

package store

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/windows"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// protectStateDir gives the state directory a protected DACL with two
// inheritable full-control ACEs, one for the private user and one for SYSTEM.
// The private user is the process user, except that a Worker that runs as the
// SYSTEM service grants the user pinned in the service environment
// ([identity.PrivateUser]), so that the installing user keeps access to the
// state of their own Worker.
// SQLite's Windows VFS creates worker.db-wal and worker.db-shm with default
// security attributes, and sqlmigrate creates backups/ and the backup files the
// same way, so all of them inherit this ACL instead of the parent directory's.
// It runs whether Open created the directory or it already existed, because
// the configuration or the identity code may have created it first.
func protectStateDir(dir string) error {
	if err := applyPrivateDACL(dir); err != nil {
		return fmt.Errorf("store: protect state directory %q: %w", dir, err)
	}
	return nil
}

func applyPrivateDACL(dir string) error {
	owner, err := identity.PrivateUser()
	if err != nil {
		return err
	}
	return applyPrivateDACLFor(dir, owner)
}

// applyPrivateDACLFor is [applyPrivateDACL] for a given user SID.
func applyPrivateDACLFor(dir string, owner *windows.SID) error {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	// TrusteeValueFromSID stores the SID address as a plain uintptr, which the
	// garbage collector does not follow: pin both SIDs and keep them reachable
	// until the ACL has been built.
	var pin runtime.Pinner
	pin.Pin(owner)
	pin.Pin(system)
	defer pin.Unpin()
	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(owner),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(system),
			},
		},
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	runtime.KeepAlive(entries)
	runtime.KeepAlive(owner)
	runtime.KeepAlive(system)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
}
