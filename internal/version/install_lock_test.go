package version

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInstallLockExcludesASecondUpdate(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "minion")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := Options{ExecPath: exe, Current: "v0.9.0"}
	target, _ := ParseVersion("v0.9.1")
	inner := func() error {
		return installLocked(o, target, func() error { return nil })
	}
	if err := installLocked(o, target, inner); !errors.Is(err, ErrInstallBusy) {
		t.Fatalf("nested install: %v, want busy", err)
	}
	// Released after the install: a second one runs.
	ran := false
	if err := installLocked(o, target, func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("after release: %v ran=%v", err, ran)
	}
}

// When the installed binary already reports the target or newer (another
// update ran meanwhile), nothing is replaced.
func TestInstallRefusesWhenANewerBinaryIsInstalled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the installed binary")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "minion")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho 'version: v0.9.2'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	target, _ := ParseVersion("v0.9.1")
	ran := false
	err := installLocked(Options{ExecPath: exe, Current: "v0.9.0"}, target, func() error { ran = true; return nil })
	if err == nil || ran {
		t.Fatalf("installed over a newer binary: %v ran=%v", err, ran)
	}
}
