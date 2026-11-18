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
	src := writeFakeBinary(t, "first")
	dir := filepath.Join(t.TempDir(), "Minion")
	dst, err := installServiceBinaryIn(src, dir, sumOf("first"))
	if err != nil {
		t.Fatal(err)
	}
	if !SameWindowsPath(dst, filepath.Join(dir, "minion.exe")) {
		t.Fatalf("dst = %s", dst)
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
	if _, err := installServiceBinaryIn(writeFakeBinary(t, "second"), dir, sumOf("second")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "second" {
		t.Fatalf("content after the second install = %q", b)
	}
	// A source that does not match the verified digest changes nothing.
	if _, err := installServiceBinaryIn(writeFakeBinary(t, "planted"), dir, sumOf("third")); err == nil {
		t.Fatal("a binary that does not match the verified digest was installed")
	}
	if b, _ := os.ReadFile(dst); string(b) != "second" {
		t.Fatalf("content after a refused install = %q", b)
	}
	// Installing unverified from the copy itself keeps it.
	if _, err := installServiceBinaryIn(dst, dir, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "second" {
		t.Fatalf("content after installing from the copy = %q", b)
	}
}

func TestInstallServiceBinaryInResetsALooseExistingDirectory(t *testing.T) {
	elevated(t)
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
	dst, err := installServiceBinaryIn(writeFakeBinary(t, "new"), dir, sumOf("new"))
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range BinaryChain(dst)[:2] {
		_, info := daclOf(t, step.Path)
		if w := UntrustedWriters(info, step.Role); len(w) != 0 {
			t.Errorf("%s: writers %v", step.Path, w)
		}
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
