//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A96 review: the transcript is opened only as
// <profile>/projects/<project>/<session>.jsonl beneath the verified profile,
// never through a symbolic link that leads out of it, and never when it is not
// a regular file (a FIFO would block the callback).
func TestOpenClaudeTranscriptStaysInTheProfile(t *testing.T) {
	profile, path := titleProfile(t)
	writeLines(t, path, renameRecord("x", titleSession))
	f, id, err := openClaudeTranscript(profile, path, titleSession)
	if err != nil || id == "" {
		t.Fatalf("open = %v %q", err, id)
	}
	_ = f.Close()
	// Through a symbolic link to the profile itself: allowed (resolved).
	link := filepath.Join(t.TempDir(), "profile-link")
	if err := os.Symlink(profile, link); err != nil {
		t.Fatal(err)
	}
	if f, _, err := openClaudeTranscript(link, path, titleSession); err != nil {
		t.Fatalf("resolved profile refused: %v", err)
	} else {
		_ = f.Close()
	}

	outside := t.TempDir()
	secret := filepath.Join(outside, titleSession+".jsonl")
	writeLines(t, secret, renameRecord("secret", titleSession))
	other, _ := titleProfile(t)
	bad := map[string]string{
		"relative":      "projects/p/" + titleSession + ".jsonl",
		"unclean":       filepath.Dir(path) + "/../" + filepath.Base(filepath.Dir(path)) + "/" + titleSession + ".jsonl",
		"other session": filepath.Join(filepath.Dir(path), "other.jsonl"),
		"other profile": filepath.Join(other, "projects", "-home-user-app", titleSession+".jsonl"),
		"outside":       secret,
		"directory":     filepath.Dir(path),
		"nested":        filepath.Join(profile, "projects", "a", "b", titleSession+".jsonl"),
	}
	// A project directory that is a link out of the profile.
	if err := os.Symlink(outside, filepath.Join(profile, "projects", "escape")); err != nil {
		t.Fatal(err)
	}
	bad["linked project"] = filepath.Join(profile, "projects", "escape", titleSession+".jsonl")
	// A transcript that is a link to a file out of the profile.
	linkDir := filepath.Join(profile, "projects", "linkfile")
	_ = os.MkdirAll(linkDir, 0o700)
	if err := os.Symlink(secret, filepath.Join(linkDir, titleSession+".jsonl")); err != nil {
		t.Fatal(err)
	}
	bad["linked file"] = filepath.Join(linkDir, titleSession+".jsonl")
	for name, p := range bad {
		if f, _, err := openClaudeTranscript(profile, p, titleSession); err == nil {
			_ = f.Close()
			t.Errorf("%s: opened %s", name, p)
		}
	}
	if _, _, err := openClaudeTranscript(profile, path, "../x"); err == nil {
		t.Error("a session id with a path separator was accepted")
	}

	// A FIFO in place of the transcript is refused at once.
	fifoDir := filepath.Join(profile, "projects", "fifo")
	_ = os.MkdirAll(fifoDir, 0o700)
	fifo := filepath.Join(fifoDir, titleSession+".jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		f, _, err := openClaudeTranscript(profile, fifo, titleSession)
		if err == nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was opened as a transcript")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO blocked")
	}
	// The whole callback with a FIFO transcript returns and records nothing.
	st := &fakeTitleStore{}
	recordClaudeTitle(t.Context(), st, "g", profile, titleEvent, titleStatusLine(strp("x"), fifo))
	if st.marks != 0 || len(st.recorded) != 0 {
		t.Fatalf("FIFO recorded: %+v", st)
	}
}
