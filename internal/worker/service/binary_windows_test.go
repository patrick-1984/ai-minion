//go:build windows

package service

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/patrick-1984/ai-minion/internal/version"
)

// elevated skips a test that sets the owner to the administrators, which
// needs an elevated token (the windows-latest runner has one).
func elevated(t *testing.T) {
	t.Helper()
	if !IsElevated() {
		t.Skip("needs an elevated (administrator) token")
	}
}

// memoryIdentity keeps the recorded service directory in memory, not in the
// registry of the test machine.
func memoryIdentity(t *testing.T) *string {
	t.Helper()
	rec := new(string)
	old := dirIdentityStore
	dirIdentityStore.get = func() string { return *rec }
	dirIdentityStore.set = func(id string) error { *rec = id; return nil }
	t.Cleanup(func() { dirIdentityStore = old })
	return rec
}

// install runs installServiceBinaryIn with an activation that does nothing.
func install(src, dir, sum string) (string, error) {
	return installServiceBinaryIn(BinaryInstall{Src: src, SHA256: sum}, dir, false, func(string) error { return nil })
}

func writeFakeBinary(t *testing.T, content string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "minion.exe")
	if err := os.WriteFile(src, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return src
}

// daclOf returns the control flags and ACEs of path.
func daclOf(t *testing.T, path string) (windows.SECURITY_DESCRIPTOR_CONTROL, SecurityInfo) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	ctrl, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	info, err := securityInfoOf(sd)
	if err != nil {
		t.Fatal(err)
	}
	return ctrl, info
}

func TestInstallServiceBinaryInProtectsTheDirectoryAndTheFile(t *testing.T) {
	elevated(t)
	rec := memoryIdentity(t)
	src := writeFakeBinary(t, "first")
	dir := filepath.Join(t.TempDir(), "Minion")
	dst, err := install(src, dir, sumOf("first"))
	if err != nil {
		t.Fatal(err)
	}
	if !SameWindowsPath(dst, filepath.Join(dir, "minion.exe")) {
		t.Fatalf("dst = %s", dst)
	}
	if id, _ := dirIdentity(dir); id == "" || *rec != id {
		t.Errorf("recorded %q, directory %q", *rec, id)
	}
	if b, _ := os.ReadFile(dst); string(b) != "first" {
		t.Fatalf("content = %q", b)
	}
	for _, p := range []string{dir, dst} {
		ctrl, info := daclOf(t, p)
		if ctrl&windows.SE_DACL_PROTECTED == 0 {
			t.Errorf("%s: DACL not protected (inherits from its parent)", p)
		}
		if info.Owner != sidAdministrators {
			t.Errorf("%s: owner %s", p, info.Owner)
		}
		got := map[string]uint32{}
		for _, a := range info.DACL {
			if a.Type != aceAllowed {
				t.Errorf("%s: ACE type %d", p, a.Type)
			}
			got[a.SID] |= a.Mask
		}
		want := map[string]uint32{sidSystem: fullControl, sidAdministrators: fullControl, sidUsers: readExecute}
		if len(got) != len(want) {
			t.Errorf("%s: ACEs %v", p, got)
		}
		for sid, m := range want {
			if got[sid] != m {
				t.Errorf("%s: %s has %#x, want %#x", p, sid, got[sid], m)
			}
		}
	}
	for _, step := range BinaryChain(dst)[:2] {
		_, info := daclOf(t, step.Path)
		if w := UntrustedWriters(info, step.Role); len(w) != 0 {
			t.Errorf("%s: writers %v", step.Path, w)
		}
	}
	// The Hub's remote update (version.ReplaceBinary, run as SYSTEM) accepts
	// the directory and the file.
	if err := version.CheckTrustedExecutable(dst); err != nil {
		t.Errorf("the self-update would refuse the service binary: %v", err)
	}

	// Running it again replaces the copy.
	if _, err := install(writeFakeBinary(t, "second"), dir, sumOf("second")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "second" {
		t.Fatalf("content after the second install = %q", b)
	}
	// A source that does not match the verified digest changes nothing.
	if _, err := install(writeFakeBinary(t, "planted"), dir, sumOf("third")); err == nil {
		t.Fatal("a binary that does not match the verified digest was installed")
	}
	if b, _ := os.ReadFile(dst); string(b) != "second" {
		t.Fatalf("content after a refused install = %q", b)
	}
	// Installing unverified from the copy itself keeps it.
	if _, err := install(dst, dir, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "second" {
		t.Fatalf("content after installing from the copy = %q", b)
	}
}

func TestInstallServiceBinaryInResetsALooseExistingDirectory(t *testing.T) {
	elevated(t)
	memoryIdentity(t)
	dir := filepath.Join(t.TempDir(), "Minion")
	loose, err := windows.SecurityDescriptorFromString("D:PAI(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: loose}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	p, _ := windows.UTF16PtrFromString(dir)
	if err := windows.CreateDirectory(p, sa); err != nil {
		t.Fatal(err)
	}
	// A file that inherited "Everyone: full control".
	if err := os.WriteFile(filepath.Join(dir, "minion.exe"), []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, info := daclOf(t, filepath.Join(dir, "minion.exe")); len(UntrustedWriters(info, RoleFile)) == 0 {
		t.Fatal("the fixture is not loose")
	}
	dst, err := install(writeFakeBinary(t, "new"), dir, sumOf("new"))
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range BinaryChain(dst)[:2] {
		_, info := daclOf(t, step.Path)
		if w := UntrustedWriters(info, step.Role); len(w) != 0 {
			t.Errorf("%s: writers %v", step.Path, w)
		}
	}
	// The loose directory was not adopted: it was moved aside, with what it held.
	aside, _ := filepath.Glob(dir + ".untrusted-*")
	if len(aside) != 1 {
		t.Fatalf("moved aside: %v", aside)
	}
	if b, _ := os.ReadFile(filepath.Join(aside[0], "minion.exe")); string(b) != "old" {
		t.Errorf("the old content moved aside is %q", b)
	}
}

func TestReadSecurityReportsAJunction(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	link := filepath.Join(base, "link")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	info, err := readSecurity(link)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Reparse {
		t.Fatal("the link is not reported as a reparse point")
	}
	if w := UntrustedWriters(info, RoleAncestor); len(w) == 0 {
		t.Error("a link on the way to the binary is accepted")
	}
}

func TestCheckServiceBinaryFlagsAUserWritableBinary(t *testing.T) {
	// A binary in a directory of this user (like %LOCALAPPDATA%\Programs\minion):
	// this user owns it and may write it, whether an administrator or not.
	src := writeFakeBinary(t, "x")
	f, err := CheckServiceBinary(src)
	if err != nil {
		t.Fatal(err)
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := u.User.Sid.String()
	found := false
	for _, x := range f {
		if x.SID == me {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings %+v lack this user %s", f, me)
	}
	w := ServiceBinaryWarning(src, f)
	if !strings.Contains(w, "writable by non-administrators") {
		t.Errorf("warning = %s", w)
	}
}

func TestServiceBinaryLocationIsBelowProgramFiles(t *testing.T) {
	p, err := ServiceBinaryLocation()
	if err != nil {
		t.Fatal(err)
	}
	pf, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !SameWindowsPath(p, filepath.Join(pf, "Minion", "minion.exe")) {
		t.Errorf("location = %s", p)
	}
	if !bytes.HasPrefix([]byte(strings.ToLower(p)), []byte(strings.ToLower(pf))) {
		t.Errorf("%s is not below %s", p, pf)
	}
}

// A directory whose access list is safe now but that this installer did not
// create (its history is unknown: a handle opened while it was open to others
// may still change it) is not reused, it is replaced.
func TestInstallServiceBinaryInDoesNotReuseADirectoryItDidNotCreate(t *testing.T) {
	elevated(t)
	rec := memoryIdentity(t)
	dir := filepath.Join(t.TempDir(), "Minion")
	if created, err := createProtectedDir(dir); err != nil || !created {
		t.Fatalf("created %v, err %v", created, err)
	}
	before, _ := dirIdentity(dir)
	if _, err := install(writeFakeBinary(t, "x"), dir, sumOf("x")); err != nil {
		t.Fatal(err)
	}
	after, _ := dirIdentity(dir)
	if after == before || *rec != after {
		t.Errorf("identity before %q, after %q, recorded %q", before, after, *rec)
	}
	if aside, _ := filepath.Glob(dir + ".untrusted-*"); len(aside) != 1 {
		t.Errorf("moved aside: %v", aside)
	}
	// The directory it created and recorded is reused.
	if _, err := install(writeFakeBinary(t, "y"), dir, sumOf("y")); err != nil {
		t.Fatal(err)
	}
	if again, _ := dirIdentity(dir); again != after {
		t.Errorf("its own directory was replaced: %q, then %q", after, again)
	}
}

// Anything in the directory that others may change stops the install before
// the service is activated.
func TestInstallServiceBinaryInChecksEveryEntryBeforeActivating(t *testing.T) {
	elevated(t)
	memoryIdentity(t)
	dir := filepath.Join(t.TempDir(), "Minion")
	if _, err := install(writeFakeBinary(t, "x"), dir, sumOf("x")); err != nil {
		t.Fatal(err)
	}
	loose, err := windows.SecurityDescriptorFromString("D:PAI(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(dir, "version.dll")
	if err := os.WriteFile(planted, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := loose.DACL()
	if err := windows.SetNamedSecurityInfo(planted, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	activated := false
	_, err = installServiceBinaryIn(BinaryInstall{Src: writeFakeBinary(t, "z"), SHA256: sumOf("z")}, dir, false,
		func(string) error { activated = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "version.dll") || activated {
		t.Fatalf("err = %v, activated %v", err, activated)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "minion.exe")); string(b) != "x" {
		t.Errorf("minion.exe = %q after a refused install", b)
	}
}
