//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// testClaudeManifest is the manifest the title tests' profile holds.
var testClaudeManifest = claudeUsageIntegration{Version: 1, Binary: "/usr/bin/minion", StateDir: "/state", Grant: strings.Repeat("b", 64), Shell: "/bin/sh"}

// writeClaudeManifest writes m as the private exporter manifest of dir.
func writeClaudeManifest(t *testing.T, dir string, m claudeUsageIntegration) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, claudeUsageManifest), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// titleClaude is a Claude profile with the exporter's manifest and the path of
// the test session's transcript in it.
func titleClaude(t *testing.T) (claudeProfile, string) {
	t.Helper()
	dir, path := titleProfile(t)
	writeClaudeManifest(t, dir, testClaudeManifest)
	m := testClaudeManifest
	return claudeProfile{dir: dir, manifest: &m}, path
}

// A96 review: a name is kept only on positive evidence that the user gave it:
// the record of their own /rename <name>. A custom title written by an unnamed
// /branch (from the first prompt) or by a prompt hook, an AI title, a prompt
// that quotes the command and a /rename without argument are not evidence; the
// session then shows no name.
func TestRecordClaudeTitleNeedsTheUsersRename(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"rename", []string{promptLine("hello"), customTitle("release prep"), `{"type":"agent-name","agentName":"release prep"}`, renameRecord("release prep", titleSession)}, "release prep"},
		{"unnamed branch", []string{customTitle("fix the secret login bug"), `{"type":"agent-name","agentName":"fix the secret login bug"}`, promptLine("fix the secret login bug")}, ""},
		{"hook title", []string{promptLine("deploy with token abc"), customTitle("deploy with token abc")}, ""},
		{"quoted command", []string{promptLine("<command-name>/rename</command-name><command-args>deploy with token abc</command-args> \"local_command\""), customTitle("deploy with token abc")}, ""},
		{"rename without argument", []string{customTitle("generated name"), renameRecord("", titleSession)}, ""},
		{"rename of another session", []string{customTitle("release prep"), renameRecord("release prep", "other")}, ""},
		{"earlier rename to another name", []string{renameRecord("old name", titleSession), customTitle("prompt derived")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, path := titleClaude(t)
			writeLines(t, path, tc.lines...)
			title := tc.want
			if title == "" {
				title = "x"
				for _, l := range tc.lines {
					var e struct{ CustomTitle string }
					if json.Unmarshal([]byte(l), &e) == nil && e.CustomTitle != "" {
						title = e.CustomTitle
					}
				}
			}
			st := &fakeTitleStore{}
			recordClaudeTitle(ctx, st, "g", profile, observedAt(1), titleStatusLine(strp(title), path))
			if st.title != tc.want {
				t.Fatalf("shown %q, want %q (confirmed %q)", st.title, tc.want, st.confirmed)
			}
			if tc.want == "" && st.marks != 1 {
				t.Fatalf("an unconfirmed name was not marked: %d", st.marks)
			}
		})
	}
}

// A96 review: an unconfirmed name is searched again only in what the
// transcript gained since (so the record that follows a custom title a moment
// later is found on the next redraw), never from the start again; while the
// transcript cannot be opened no name is shown and the next redraw tries
// again. A known name that the session no longer has is cleared, and a repeat
// of the confirmed name needs no new search.
func TestRecordClaudeTitleRetriesOnTranscriptGrowth(t *testing.T) {
	ctx := t.Context()
	profile, path := titleClaude(t)
	st := &fakeTitleStore{}
	line := titleStatusLine(strp("release prep"), path)
	tick := 0
	record := func(raw []byte) {
		tick++
		recordClaudeTitle(ctx, st, "g", profile, observedAt(tick), raw)
	}

	// Missing transcript: nothing searched, nothing shown.
	record(line)
	if st.marks != 0 || len(st.confirmed) != 0 || st.title != "" {
		t.Fatalf("transient failure cached: %+v", st)
	}
	// The custom title is written before the command record.
	writeLines(t, path, promptLine("hi"), customTitle("release prep"))
	record(line)
	info, _ := os.Stat(path)
	if st.marks != 1 || st.scanned != info.Size() || st.title != "" {
		t.Fatalf("first search = %+v", st)
	}
	// No change: not searched again.
	record(line)
	if st.marks != 1 {
		t.Fatalf("unchanged transcript searched again: %d", st.marks)
	}
	// A torn last line is left for the next search.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	rec := renameRecord("release prep", titleSession)
	_, _ = f.WriteString(rec[:20])
	record(line)
	if st.scanned != info.Size() || st.title != "" {
		t.Fatalf("torn line = %+v", st)
	}
	_, _ = f.WriteString(rec[20:] + "\n")
	_ = f.Close()
	record(line)
	if st.title != "release prep" {
		t.Fatalf("rename after growth not found: %+v", st)
	}
	// The session reports a prompt-derived name later: the known one goes.
	appendLines(t, path, customTitle("deploy with token abc"))
	record(titleStatusLine(strp("deploy with token abc"), path))
	if st.title != "" {
		t.Fatalf("a hook-set name kept %q", st.title)
	}
	// Back to the user's name: shown again without a search.
	confirmed := len(st.confirmed)
	record(line)
	if st.title != "release prep" || len(st.confirmed) != confirmed {
		t.Fatalf("user's name again = %+v", st)
	}
	// A different name whose transcript cannot be read clears the shown one.
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatal(err)
	}
	record(titleStatusLine(strp("another name"), path))
	if st.title != "" {
		t.Fatalf("an unreadable new name left %q shown", st.title)
	}
	if err := os.Rename(path+".moved", path); err != nil {
		t.Fatal(err)
	}
	// No session_name: no name.
	record(line)
	record(titleStatusLine(nil, path))
	if st.title != "" {
		t.Fatalf("absent name kept %q", st.title)
	}
}

// A96 review round 2: an older callback that finishes last cannot show its
// name after a newer observation (a clear here, a different name or a
// pending check alike): the confirmation is shown only while its own
// observation is the latest.
func TestRecordClaudeTitleOlderCallbackCannotRestoreAName(t *testing.T) {
	ctx := t.Context()
	profile, path := titleClaude(t)
	writeLines(t, path, renameRecord("release prep", titleSession))
	st := &fakeTitleStore{}
	older, newer := observedAt(1), observedAt(2)
	// The older callback observed the name and is still searching when the
	// newer one, with no name, is recorded.
	obs, _ := st.ObserveHarnessTitle(ctx, "g", older, "release prep")
	if !obs.Verify {
		t.Fatal("a new name needs a search")
	}
	recordClaudeTitle(ctx, st, "g", profile, newer, titleStatusLine(nil, path))
	if err := st.ConfirmHarnessTitle(ctx, "g", older, "release prep"); err != nil {
		t.Fatal(err)
	}
	if st.title != "" {
		t.Fatalf("an older callback restored %q", st.title)
	}
	// The whole older callback, recorded last, changes nothing either.
	recordClaudeTitle(ctx, st, "g", profile, older, titleStatusLine(strp("release prep"), path))
	if st.title != "" {
		t.Fatalf("a stale callback showed %q", st.title)
	}
}

// A96 review round 2: the transcript search stops early enough to record how
// far it got within the callback's budget.
func TestClaudeTitleScanLeavesTimeToCommit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	parent, _ := ctx.Deadline()
	scan, stop := claudeTitleScanContext(ctx)
	defer stop()
	d, ok := scan.Deadline()
	if !ok || parent.Sub(d) < claudeTitleCommitTime {
		t.Fatalf("scan deadline %v, parent %v", d, parent)
	}
	// A search under an expired budget records the progress it made under
	// the callback's own context.
	profile, path := titleClaude(t)
	writeLines(t, path, promptLine("hi"))
	short, cancelShort := context.WithTimeout(t.Context(), claudeTitleCommitTime/2)
	defer cancelShort()
	st := &fakeTitleStore{}
	recordClaudeTitle(short, st, "g", profile, observedAt(1), titleStatusLine(strp("x"), path))
	if st.title != "" || len(st.confirmed) != 0 {
		t.Fatalf("expired search = %+v", st)
	}
}

// A96 review: the transcript is opened only as
// <profile>/projects/<project>/<session>.jsonl beneath the profile directory
// that holds the exporter's manifest, never through a symbolic link that leads
// out of it, and never when it is not a regular file (a FIFO would block the
// callback).
func TestOpenClaudeTranscriptStaysInTheProfile(t *testing.T) {
	profile, path := titleClaude(t)
	writeLines(t, path, renameRecord("x", titleSession))
	f, id, err := openClaudeTranscript(profile, path, titleSession)
	if err != nil || id == "" {
		t.Fatalf("open = %v %q", err, id)
	}
	_ = f.Close()
	// Through a symbolic link to the profile itself: allowed (resolved).
	link := filepath.Join(t.TempDir(), "profile-link")
	if err := os.Symlink(profile.dir, link); err != nil {
		t.Fatal(err)
	}
	linked := profile
	linked.dir = link
	if f, _, err := openClaudeTranscript(linked, path, titleSession); err != nil {
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
		"nested":        filepath.Join(profile.dir, "projects", "a", "b", titleSession+".jsonl"),
	}
	// A project directory that is a link out of the profile.
	if err := os.Symlink(outside, filepath.Join(profile.dir, "projects", "escape")); err != nil {
		t.Fatal(err)
	}
	bad["linked project"] = filepath.Join(profile.dir, "projects", "escape", titleSession+".jsonl")
	// A transcript that is a link to a file out of the profile.
	linkDir := filepath.Join(profile.dir, "projects", "linkfile")
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
	fifoDir := filepath.Join(profile.dir, "projects", "fifo")
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
	// The whole callback with a FIFO transcript returns and shows nothing.
	st := &fakeTitleStore{}
	recordClaudeTitle(t.Context(), st, "g", profile, observedAt(1), titleStatusLine(strp("x"), fifo))
	if st.marks != 0 || len(st.confirmed) != 0 || st.title != "" {
		t.Fatalf("FIFO recorded: %+v", st)
	}
}

// A96 review round 2: the profile directory is resolved once and the
// transcript is opened only while that very directory holds the exporter's
// manifest: a profile alias retargeted to a directory without it (or with
// another grant's manifest, or a link in its place) is refused.
func TestOpenClaudeTranscriptNeedsTheManifestInTheHeldProfile(t *testing.T) {
	profile, path := titleClaude(t)
	writeLines(t, path, renameRecord("x", titleSession))

	// An alias retargeted to another profile with a transcript of the same
	// shape but without the manifest.
	other, otherPath := titleProfile(t)
	writeLines(t, otherPath, renameRecord("secret", titleSession))
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	moved := profile
	moved.dir = alias
	if f, _, err := openClaudeTranscript(moved, filepath.Join(alias, "projects", "-home-user-app", titleSession+".jsonl"), titleSession); err == nil {
		_ = f.Close()
		t.Fatal("a profile without the manifest was read")
	}
	// Another grant's manifest.
	foreign := testClaudeManifest
	foreign.Grant = strings.Repeat("c", 64)
	writeClaudeManifest(t, other, foreign)
	if f, _, err := openClaudeTranscript(moved, filepath.Join(alias, "projects", "-home-user-app", titleSession+".jsonl"), titleSession); err == nil {
		_ = f.Close()
		t.Fatal("a profile with another grant's manifest was read")
	}
	// A link in place of the manifest.
	if err := os.Remove(filepath.Join(other, claudeUsageManifest)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(profile.dir, claudeUsageManifest), filepath.Join(other, claudeUsageManifest)); err != nil {
		t.Fatal(err)
	}
	if f, _, err := openClaudeTranscript(moved, filepath.Join(alias, "projects", "-home-user-app", titleSession+".jsonl"), titleSession); err == nil {
		_ = f.Close()
		t.Fatal("a linked manifest was accepted")
	}
	// A manifest others can read is refused.
	if err := os.Chmod(filepath.Join(profile.dir, claudeUsageManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if f, _, err := openClaudeTranscript(profile, path, titleSession); err == nil {
		_ = f.Close()
		t.Fatal("a manifest that is not private was accepted")
	}
	// No manifest known: nothing is opened.
	if _, _, err := openClaudeTranscript(claudeProfile{dir: profile.dir}, path, titleSession); err == nil {
		t.Fatal("opened without a manifest")
	}
}

var _ claudeTitleStore = (*store.Store)(nil)
