//go:build windows

package version

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// These tests build synthetic directory ACLs on throw-away directories below
// t.TempDir (nothing outside it is touched) and check what the trusted-object
// boundary of the update path does with them. "Untrusted" stands for Everyone,
// a principal that is never in the trusted set.

func aclTrustee(sid *windows.SID, typ windows.TRUSTEE_TYPE) windows.TRUSTEE {
	return windows.TRUSTEE{
		TrusteeForm:  windows.TRUSTEE_IS_SID,
		TrusteeType:  typ,
		TrusteeValue: windows.TrusteeValueFromSID(sid),
	}
}

func aclWellKnown(t *testing.T, w windows.WELL_KNOWN_SID_TYPE) *windows.SID {
	t.Helper()
	sid, err := windows.CreateWellKnownSid(w)
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

func aclCurrentUser(t *testing.T) *windows.SID {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid
}

// aclEntry grants access to sid with the given ACE inheritance flags.
func aclEntry(sid *windows.SID, typ windows.TRUSTEE_TYPE, access, inheritance uint32) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.ACCESS_MASK(access),
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee:           aclTrustee(sid, typ),
	}
}

// aclTrustedEntries are the entries every fixture has: the running user and
// SYSTEM with full control over the directory and everything created in it.
func aclTrustedEntries(t *testing.T) []windows.EXPLICIT_ACCESS {
	t.Helper()
	return []windows.EXPLICIT_ACCESS{
		aclEntry(aclCurrentUser(t), windows.TRUSTEE_IS_USER, windows.GENERIC_ALL, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
		aclEntry(aclWellKnown(t, windows.WinLocalSystemSid), windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_ALL, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
	}
}

// aclSetProtected replaces the DACL of dir with entries and stops it from
// inheriting, so only the entries given exist on it.
func aclSetProtected(t *testing.T, dir string, entries ...windows.EXPLICIT_ACCESS) {
	t.Helper()
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

// aclDir creates a directory below parent that holds the trusted entries plus
// extra.
func aclDir(t *testing.T, parent, name string, extra ...windows.EXPLICIT_ACCESS) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	aclSetProtected(t, dir, append(aclTrustedEntries(t), extra...)...)
	return dir
}

func aclEveryone(t *testing.T, access, inheritance uint32) windows.EXPLICIT_ACCESS {
	return aclEntry(aclWellKnown(t, windows.WinWorldSid), windows.TRUSTEE_IS_WELL_KNOWN_GROUP, access, inheritance)
}

const (
	aclWrite   = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA
	aclRead    = windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE
	aclContObj = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
)

// TestCheckTrustedDirInheritableACEs: an ACE that is inherit-only on the
// checked leaf does not apply to the leaf, but every directory and file this
// program creates in it receives it. The leaf is refused when such an ACE
// gives write access to an untrusted principal, and accepted when it does not.
func TestCheckTrustedDirInheritableACEs(t *testing.T) {
	cases := []struct {
		name  string
		extra func(*testing.T) windows.EXPLICIT_ACCESS
		bad   bool
	}{
		{"inherit-only write to new subdirectories and files", func(t *testing.T) windows.EXPLICIT_ACCESS {
			return aclEveryone(t, aclWrite, aclContObj|windows.INHERIT_ONLY)
		}, true},
		{"inherit-only write to new files only", func(t *testing.T) windows.EXPLICIT_ACCESS {
			return aclEveryone(t, aclWrite, windows.SUB_OBJECTS_ONLY_INHERIT|windows.INHERIT_ONLY)
		}, true},
		{"inherit-only write to new subdirectories only", func(t *testing.T) windows.EXPLICIT_ACCESS {
			return aclEveryone(t, aclWrite, windows.SUB_CONTAINERS_ONLY_INHERIT|windows.INHERIT_ONLY)
		}, true},
		{"inherit-only write that stops after one level", func(t *testing.T) windows.EXPLICIT_ACCESS {
			return aclEveryone(t, aclWrite, aclContObj|windows.INHERIT_NO_PROPAGATE|windows.INHERIT_ONLY)
		}, true},
		{"inherit-only generic write", func(t *testing.T) windows.EXPLICIT_ACCESS {
			return aclEveryone(t, windows.GENERIC_WRITE, aclContObj|windows.INHERIT_ONLY)
		}, true},
		{"inherit-only change of the DACL", func(t *testing.T) windows.EXPLICIT_ACCESS {
			return aclEveryone(t, windows.WRITE_DAC, aclContObj|windows.INHERIT_ONLY)
		}, true},
		{"effective inheritable write (applies to the leaf itself)", func(t *testing.T) windows.EXPLICIT_ACCESS {
			return aclEveryone(t, aclWrite, aclContObj)
		}, true},
		{"inherit-only read and execute", func(t *testing.T) windows.EXPLICIT_ACCESS {
			return aclEveryone(t, aclRead, aclContObj|windows.INHERIT_ONLY)
		}, false},
		{"effective inheritable read and execute", func(t *testing.T) windows.EXPLICIT_ACCESS {
			return aclEveryone(t, aclRead, aclContObj)
		}, false},
		{"inherit-only full control for CREATOR OWNER", func(t *testing.T) windows.EXPLICIT_ACCESS {
			return aclEntry(aclWellKnown(t, windows.WinCreatorOwnerSid), windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_ALL, aclContObj|windows.INHERIT_ONLY)
		}, false},
		{"inherit-only full control for SYSTEM", func(t *testing.T) windows.EXPLICIT_ACCESS {
			return aclEntry(aclWellKnown(t, windows.WinLocalSystemSid), windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_ALL, aclContObj|windows.INHERIT_ONLY)
		}, false},
		{"inherit-only full control for Administrators", func(t *testing.T) windows.EXPLICIT_ACCESS {
			return aclEntry(aclWellKnown(t, windows.WinBuiltinAdministratorsSid), windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_ALL, aclContObj|windows.INHERIT_ONLY)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leaf := aclDir(t, t.TempDir(), "leaf", tc.extra(t))
			err := checkTrustedDir(leaf)
			switch {
			case tc.bad && !errors.Is(err, ErrUntrustedCache):
				t.Errorf("err = %v, want ErrUntrustedCache", err)
			case !tc.bad && err != nil:
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}

// TestCheckTrustedDirAncestorInheritOnly: an inherit-only ACE on a directory
// above a protected leaf cannot reach the leaf, because inheritance happens
// when an object is created, so it is not a reason to refuse the leaf. The
// same ancestor ACE does matter for a leaf that did inherit it.
func TestCheckTrustedDirAncestorInheritOnly(t *testing.T) {
	parent := aclDir(t, t.TempDir(), "parent", aclEveryone(t, aclWrite, aclContObj|windows.INHERIT_ONLY))
	if err := checkTrustedDir(parent); !errors.Is(err, ErrUntrustedCache) {
		t.Errorf("the directory that carries the inherit-only ACE is a leaf here and must be refused: %v", err)
	}

	protected := aclDir(t, parent, "protected")
	if err := checkTrustedDir(protected); err != nil {
		t.Errorf("protected leaf below an ancestor with an inherit-only write ACE: %v", err)
	}

	inherited := filepath.Join(parent, "inherited")
	if err := os.Mkdir(inherited, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkTrustedDir(inherited); !errors.Is(err, ErrUntrustedCache) {
		t.Errorf("leaf that inherited the write ACE: %v", err)
	}
}

// TestCacheStagingBoundary: a cache root whose inheritable ACEs would give an
// untrusted principal write access to the staging directory is refused before
// anything is created in it; a protected one yields a staging directory that
// passes the same check and holds the authenticated files.
func TestCacheStagingBoundary(t *testing.T) {
	t.Run("unsafe inherit-only root", func(t *testing.T) {
		root := aclDir(t, t.TempDir(), "cache", aclEveryone(t, aclWrite, aclContObj|windows.INHERIT_ONLY))
		c := Cache{Dir: root}
		if _, err := c.NewStaging(); !errors.Is(err, ErrUntrustedCache) {
			t.Fatalf("NewStaging err = %v, want ErrUntrustedCache", err)
		}
		if err := c.Prepare(); !errors.Is(err, ErrUntrustedCache) {
			t.Errorf("Prepare err = %v, want ErrUntrustedCache", err)
		}
		if ents, _ := os.ReadDir(root); len(ents) != 0 {
			t.Errorf("an unsafe cache root got entries: %v", ents)
		}
	})
	t.Run("protected root", func(t *testing.T) {
		root := aclDir(t, t.TempDir(), "cache")
		c := Cache{Dir: root}
		staging, err := c.NewStaging()
		if err != nil {
			t.Fatalf("NewStaging: %v", err)
		}
		if err := checkTrustedDir(staging); err != nil {
			t.Errorf("staging directory: %v", err)
		}
		if err := writeMeta(staging, CacheMeta{Tag: "v0.5.0"}); err != nil {
			t.Fatalf("writeMeta in staging: %v", err)
		}
		if err := c.Commit(staging, "v0.5.0"); err != nil {
			t.Fatal(err)
		}
		if _, err := readMeta(c.EntryDir("v0.5.0")); err != nil {
			t.Errorf("a committed entry is not readable through the trusted open: %v", err)
		}
	})
}

// TestNewFileInheritedACL: a file created in a directory that hands write
// access to someone else is refused on its open handle, before a byte is
// written; one created in a protected directory is accepted.
func TestNewFileInheritedACL(t *testing.T) {
	unsafe := aclDir(t, t.TempDir(), "unsafe", aclEveryone(t, aclWrite, windows.SUB_OBJECTS_ONLY_INHERIT|windows.INHERIT_ONLY))
	f, err := os.OpenFile(filepath.Join(unsafe, "archive"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkNewFile(f); !errors.Is(err, ErrUntrustedCache) {
		t.Errorf("file that inherited a write ACE: %v", err)
	}
	_ = f.Close()

	safe := aclDir(t, t.TempDir(), "safe", aclEveryone(t, aclRead, aclContObj|windows.INHERIT_ONLY))
	g, err := os.OpenFile(filepath.Join(safe, "archive"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	if err := checkNewFile(g); err != nil {
		t.Errorf("file in a protected directory: %v", err)
	}

	// writeFileAtomic goes through the same check and leaves nothing behind.
	if err := writeFileAtomic(filepath.Join(unsafe, "meta.json"), []byte("{}"), 0o600); !errors.Is(err, ErrUntrustedCache) {
		t.Errorf("writeFileAtomic into an unsafe directory: %v", err)
	}
	if ents, _ := os.ReadDir(unsafe); len(ents) != 1 {
		t.Errorf("unsafe directory holds %d entries, want only the file created above", len(ents))
	}
	if err := writeFileAtomic(filepath.Join(safe, "meta.json"), []byte("{}"), 0o600); err != nil {
		t.Errorf("writeFileAtomic into a protected directory: %v", err)
	}
}

// TestReplaceBinaryInstallDirBoundary: the verified executable is only placed
// in a directory that trusted principals alone can change, whatever the
// state of the source file.
func TestReplaceBinaryInstallDirBoundary(t *testing.T) {
	src := filepath.Join(aclDir(t, t.TempDir(), "src"), "new")
	writeFile(t, src, "new", 0o600)

	t.Run("unsafe inherit-only install directory", func(t *testing.T) {
		dir := aclDir(t, t.TempDir(), "bin", aclEveryone(t, aclWrite, aclContObj|windows.INHERIT_ONLY))
		target := filepath.Join(dir, "minion.exe")
		writeFile(t, target, "old", 0o755)
		err := ReplaceBinary(target, src, "windows", sha256Hex("new"))
		if !errors.Is(err, ErrUntrustedCache) {
			t.Fatalf("err = %v, want ErrUntrustedCache", err)
		}
		if got := readFile(t, target); got != "old" {
			t.Errorf("target = %q, want it untouched", got)
		}
		ents, _ := os.ReadDir(dir)
		if len(ents) != 1 {
			t.Errorf("install directory holds %d entries, want only the target: %v", len(ents), ents)
		}
	})
	t.Run("unsafe ancestor that can rename the install directory", func(t *testing.T) {
		parent := aclDir(t, t.TempDir(), "parent", aclEveryone(t, windows.DELETE|fileDeleteChild, windows.NO_INHERITANCE))
		dir := aclDir(t, parent, "bin")
		target := filepath.Join(dir, "minion.exe")
		writeFile(t, target, "old", 0o755)
		if err := ReplaceBinary(target, src, "windows", sha256Hex("new")); !errors.Is(err, ErrUntrustedCache) {
			t.Fatalf("err = %v, want ErrUntrustedCache", err)
		}
		if got := readFile(t, target); got != "old" {
			t.Errorf("target = %q, want it untouched", got)
		}
	})
	t.Run("protected install directory", func(t *testing.T) {
		// Read access for everyone, an inheritable CREATOR OWNER entry and an
		// inherit-only ACE on an ancestor are all fine.
		parent := aclDir(t, t.TempDir(), "parent", aclEveryone(t, aclWrite, aclContObj|windows.INHERIT_ONLY))
		dir := aclDir(t, parent, "bin",
			aclEveryone(t, aclRead, aclContObj),
			aclEntry(aclWellKnown(t, windows.WinCreatorOwnerSid), windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_ALL, aclContObj|windows.INHERIT_ONLY))
		target := filepath.Join(dir, "minion.exe")
		writeFile(t, target, "old", 0o755)
		if err := ReplaceBinary(target, src, "windows", sha256Hex("bad")); !errors.Is(err, ErrChecksum) {
			t.Fatalf("wrong digest: err = %v, want ErrChecksum", err)
		}
		if got := readFile(t, target); got != "old" {
			t.Errorf("target after a digest mismatch = %q", got)
		}
		if err := ReplaceBinary(target, src, "windows", sha256Hex("new")); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, target); got != "new" {
			t.Errorf("target = %q", got)
		}
		if got := readFile(t, target+".old"); got != "old" {
			t.Errorf("previous executable = %q", got)
		}
		if err := checkTrustedDir(dir); err != nil {
			t.Errorf("install directory after the update: %v", err)
		}
	})
}

// TestDefaultUserInstallPath: the ordinary layout (a private directory under
// the user's temp or profile, default inherited ACLs) is accepted end to end.
func TestDefaultUserInstallPath(t *testing.T) {
	base := t.TempDir()
	cache := Cache{Dir: filepath.Join(base, "cache")}
	if err := cache.Prepare(); err != nil {
		t.Fatalf("Prepare with default ACLs: %v", err)
	}
	staging, err := cache.NewStaging()
	if err != nil {
		t.Fatalf("NewStaging with default ACLs: %v", err)
	}
	bin := filepath.Join(staging, "minion.exe")
	writeFile(t, bin, "new", 0o700)
	target := filepath.Join(base, "minion.exe")
	writeFile(t, target, "old", 0o755)
	if err := ReplaceBinary(target, bin, "windows", sha256Hex("new")); err != nil {
		t.Fatalf("ReplaceBinary with default ACLs: %v", err)
	}
	if got := readFile(t, target); got != "new" {
		t.Errorf("target = %q", got)
	}
}
