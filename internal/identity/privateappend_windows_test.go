//go:build windows

package identity

import (
	"os"
	"path/filepath"
	"testing"
)

// A log created by OpenPrivateAppend passes the strict private check, and a
// file that was created with an inherited (readable by others) ACL is
// replaced by one that does.
func TestOpenPrivateAppendCreatesAProtectedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "worker-launch.log")
	f, err := OpenPrivateAppend(p)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("one\n")
	_ = f.Close()
	if err := CheckPrivate(p); err != nil {
		t.Fatal(err)
	}
	f, err = OpenPrivateAppend(p)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("two\n")
	_ = f.Close()
	b, _ := os.ReadFile(p)
	if string(b) != "one\ntwo\n" {
		t.Errorf("content %q", b)
	}
}
