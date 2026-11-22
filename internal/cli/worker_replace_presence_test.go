package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// The replacement keeps a private copy of the old profile; the published
// presence files (owner data of the usage document) are not part of it.
func TestCopyTreeSkipsThePresenceDirectory(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "presence"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"presence/a.json", "presence/a.usage.json", "worker.key"} {
		if err := os.WriteFile(filepath.Join(src, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(t.TempDir(), "copy")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "worker.key")); err != nil {
		t.Fatal("the key was not copied")
	}
	if _, err := os.Stat(filepath.Join(dst, "presence")); !os.IsNotExist(err) {
		t.Fatalf("presence directory was copied: %v", err)
	}
}

// A failed candidate may already have received usage from the Hub. The rollback
// clears the published files of the candidate before the old tree comes back,
// and again after it (the restored snapshot carries the old published files).
func TestReplaceRollbackClearsPublishedFilesOfTheCandidateAndOfTheSnapshot(t *testing.T) {
	r := newReplaceRig(t)
	dir := r.stateDir()
	// The old profile published a usage document.
	if err := os.MkdirAll(filepath.Join(dir, "presence"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldUsage := filepath.Join(dir, "presence", "old.usage.json")
	if err := os.WriteFile(oldUsage, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	r.deps.replace.clearPublished = func(d string) error {
		state := "old"
		if _, err := os.Stat(filepath.Join(d, "candidate-marker")); err == nil {
			state = "candidate"
		}
		calls = append(calls, state)
		// What the real call does, for a user profile.
		return os.RemoveAll(filepath.Join(d, "presence"))
	}
	r.hook("enroll", func(agent.Settings) error {
		// The candidate received usage and published it, then the join fails.
		if err := os.WriteFile(filepath.Join(dir, "candidate-marker"), []byte("x"), 0o600); err != nil {
			return err
		}
		return errors.New("the invitation was refused")
	})
	_, err := r.run(t, "--replace")
	if err == nil || !strings.Contains(err.Error(), "old Worker was kept") {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(calls, ","); got != "old,candidate,old" {
		t.Fatalf("clear calls = %s, want old (before the move), candidate (before the restore), old (after it)", got)
	}
	if _, err := os.Stat(oldUsage); !os.IsNotExist(err) {
		t.Fatalf("the restored snapshot kept the old published usage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "candidate-marker")); err == nil {
		t.Fatal("the candidate's files are still there")
	}
	r.oldIntact(t)
}

// With the real clearing, a profile's published files go before the move and
// again on rollback.
func TestReplaceClearsRealPublishedFiles(t *testing.T) {
	r := newReplaceRig(t)
	dir := r.stateDir()
	_ = os.MkdirAll(filepath.Join(dir, "presence"), 0o700)
	usage := filepath.Join(dir, "presence", "old.usage.json")
	if err := os.WriteFile(usage, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.hook("enroll", func(agent.Settings) error { return errors.New("refused") })
	if _, err := r.run(t, "--replace"); err == nil {
		t.Fatal("expected a failure")
	}
	if _, err := os.Stat(usage); !os.IsNotExist(err) {
		t.Fatalf("usage kept: %v", err)
	}
}
