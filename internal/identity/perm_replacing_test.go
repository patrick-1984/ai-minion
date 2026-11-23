package identity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReplacingKeepsAnExactBackupAndReportsAConcurrentWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteReplacing(path, []byte(`{"a":2}`), nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path + BackupSuffix)
	if string(got) != `{"a":1}` {
		t.Fatalf("backup %q", got)
	}
	if info, _ := os.Stat(path + BackupSuffix); info.Mode().Perm() != 0o600 && !isWindowsLike() {
		t.Fatalf("backup mode %v", info.Mode())
	}
	// A later write replaces the previous backup.
	if err := WriteReplacing(path, []byte(`{"a":3}`), nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path + BackupSuffix); string(got) != `{"a":2}` {
		t.Fatalf("second backup %q", got)
	}
	// A change between the read and the replacement writes nothing.
	err := WriteReplacing(path, []byte(`{"a":4}`), func() error {
		return os.WriteFile(path, []byte(`{"a":"claude"}`), 0o600)
	})
	if !errors.Is(err, ErrChangedDuringWrite) {
		t.Fatalf("err %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != `{"a":"claude"}` {
		t.Fatalf("a concurrent save was overwritten: %q", got)
	}
	// A new file has nothing to back up.
	fresh := filepath.Join(dir, "new.json")
	if err := WriteReplacing(fresh, []byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh + BackupSuffix); err == nil {
		t.Fatal("backup of a file that did not exist")
	}
}

func isWindowsLike() bool { return filepath.Separator == '\\' }
