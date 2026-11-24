package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

const titleSession = "0f6a3c2e-1111-4222-8333-944455556666"

// titleProfile is a Claude profile directory with one project directory.
func titleProfile(t *testing.T) (profile, path string) {
	t.Helper()
	profile = t.TempDir()
	dir := filepath.Join(profile, "projects", "-home-user-app")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return profile, filepath.Join(dir, titleSession+".jsonl")
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

func jsonLine(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// The records Claude Code 2.1 writes (seen in real transcripts): /rename <x>
// appends custom-title, agent-name and then the local_command record of the
// user's command; an unnamed /branch and prompt hooks append only the
// custom-title.
func customTitle(title string) string {
	return jsonLine(map[string]any{"type": "custom-title", "customTitle": title, "sessionId": titleSession})
}

func renameRecord(arg, session string) string {
	return jsonLine(map[string]any{"type": "system", "subtype": "local_command", "isMeta": false, "level": "info", "sessionId": session,
		"content": "<command-name>/rename</command-name>\n            <command-message>rename</command-message>\n            <command-args>" + arg + "</command-args>"})
}

func promptLine(text string) string {
	return jsonLine(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}, "sessionId": titleSession})
}

type fakeTitleStore struct {
	kept     store.HarnessTitleCheck
	recorded []string
	marks    int
}

func (f *fakeTitleStore) StoredHarnessTitle(context.Context, string, string, string, int64, string) (store.HarnessTitleCheck, error) {
	return f.kept, nil
}

func (f *fakeTitleStore) RecordHarnessTitle(_ context.Context, _ string, _ store.HarnessEvent, title string) error {
	f.recorded = append(f.recorded, title)
	f.kept = store.HarnessTitleCheck{Title: title}
	return nil
}

func (f *fakeTitleStore) MarkHarnessTitleChecked(_ context.Context, _ string, _ store.HarnessEvent, digest string, scanned int64) error {
	f.marks++
	f.kept = store.HarnessTitleCheck{Checked: digest, Scanned: scanned}
	return nil
}

func titleStatusLine(name *string, transcript string) []byte {
	m := map[string]any{"session_id": titleSession, "transcript_path": transcript}
	if name != nil {
		m["session_name"] = *name
	}
	b, _ := json.Marshal(m)
	return b
}

func strp(s string) *string { return &s }

var titleEvent = store.HarnessEvent{Session: "work", Pane: "%1", Created: 1, Harness: "claude", NativeID: strings.Repeat("a", 64)}

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
			profile, path := titleProfile(t)
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
			recordClaudeTitle(ctx, st, "g", profile, titleEvent, titleStatusLine(strp(title), path))
			if st.kept.Title != tc.want {
				t.Fatalf("kept %q, want %q (recorded %q)", st.kept.Title, tc.want, st.recorded)
			}
			if tc.want == "" && st.marks != 1 {
				t.Fatalf("an unconfirmed name was not marked: %d", st.marks)
			}
		})
	}
}

// A96 review: an unconfirmed name is searched again only in what the
// transcript gained since (so the record that follows a custom title a moment
// later is found on the next redraw), never from the start again, and a
// transcript that could not be opened (a transient failure) records nothing,
// so the next redraw tries again. A known name that the session no longer has
// is cleared.
func TestRecordClaudeTitleRetriesOnTranscriptGrowth(t *testing.T) {
	ctx := t.Context()
	profile, path := titleProfile(t)
	st := &fakeTitleStore{}
	line := titleStatusLine(strp("release prep"), path)

	// Missing transcript: nothing recorded.
	recordClaudeTitle(ctx, st, "g", profile, titleEvent, line)
	if st.marks != 0 || len(st.recorded) != 0 {
		t.Fatalf("transient failure cached: marks %d, recorded %q", st.marks, st.recorded)
	}
	// The custom title is written before the command record.
	writeLines(t, path, promptLine("hi"), customTitle("release prep"))
	recordClaudeTitle(ctx, st, "g", profile, titleEvent, line)
	info, _ := os.Stat(path)
	if st.marks != 1 || st.kept.Scanned != info.Size() || st.kept.Title != "" {
		t.Fatalf("first search = %+v (marks %d)", st.kept, st.marks)
	}
	// No change: not searched again.
	recordClaudeTitle(ctx, st, "g", profile, titleEvent, line)
	if st.marks != 1 {
		t.Fatalf("unchanged transcript searched again: %d", st.marks)
	}
	// A torn last line is left for the next search.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	rec := renameRecord("release prep", titleSession)
	_, _ = f.WriteString(rec[:20])
	recordClaudeTitle(ctx, st, "g", profile, titleEvent, line)
	if st.kept.Scanned != info.Size() || st.kept.Title != "" {
		t.Fatalf("torn line = %+v", st.kept)
	}
	_, _ = f.WriteString(rec[20:] + "\n")
	_ = f.Close()
	recordClaudeTitle(ctx, st, "g", profile, titleEvent, line)
	if st.kept.Title != "release prep" {
		t.Fatalf("rename after growth not found: %+v", st.kept)
	}
	// The session reports a prompt-derived name later: the known one goes.
	appendLines(t, path, customTitle("deploy with token abc"))
	recordClaudeTitle(ctx, st, "g", profile, titleEvent, titleStatusLine(strp("deploy with token abc"), path))
	if st.kept.Title != "" {
		t.Fatalf("a hook-set name kept %q", st.kept.Title)
	}
	// Back to the user's name: the record is in the transcript already.
	recordClaudeTitle(ctx, st, "g", profile, titleEvent, line)
	if st.kept.Title != "release prep" {
		t.Fatalf("user's name again = %+v", st.kept)
	}
	// No session_name: no name.
	recordClaudeTitle(ctx, st, "g", profile, titleEvent, titleStatusLine(nil, path))
	if st.kept.Title != "" {
		t.Fatalf("absent name kept %q", st.kept.Title)
	}
}

// A96 review: a long transcript is searched in bounded steps that resume where
// the last one stopped, also inside a long line, whose rest is never decoded.
func TestClaudeRenamedResumesInBoundedSteps(t *testing.T) {
	ctx := t.Context()
	_, path := titleProfile(t)
	huge := promptLine(strings.Repeat("p", maxClaudeTitleLine*2) + "\n" + renameRecord("fake", titleSession))
	writeLines(t, path, huge, renameRecord("release prep", titleSession))
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, _ := f.Stat()
	// Stop inside the long line, then resume there.
	short, cancel := context.WithCancel(ctx)
	cancel()
	if found, pos, err := claudeRenamed(short, f, 0, info.Size(), titleSession, "release prep"); found || pos != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled search = %v %d %v", found, pos, err)
	}
	mid := int64(maxClaudeTitleLine + 10)
	if found, _, err := claudeRenamed(ctx, f, mid, info.Size(), titleSession, "fake"); found || err != nil {
		t.Fatalf("the rest of a long line was decoded: %v %v", found, err)
	}
	if found, pos, err := claudeRenamed(ctx, f, mid, info.Size(), titleSession, "release prep"); !found || err != nil || pos != info.Size() {
		t.Fatalf("resumed search = %v %d %v", found, pos, err)
	}
}

// A96 review: --name / -n on the claude command line is evidence too.
func TestClaudeNameArg(t *testing.T) {
	for argv, want := range map[string]string{
		"claude --name release prep":     "release",
		"claude --name release":          "release",
		"claude -n first --name=second":  "second",
		"claude -p hello":                "",
		"claude -- --name x":             "",
		"claude --resume id -n x\x1b[2J": "x[2J",
	} {
		if got := claudeNameArg(strings.Fields(argv)); got != want {
			t.Errorf("%q: %q, want %q", argv, got, want)
		}
	}
	if got := claudeNameArg([]string{"claude", "--name", "release prep"}); got != "release prep" {
		t.Errorf("spaced name = %q", got)
	}
	if runtime.GOOS == "linux" {
		pid := os.Getpid()
		if claudeStartedWithName(pid, agentdetect.ProcessBirth(pid), "anything") {
			t.Error("the test binary was read as started with --name")
		}
		if claudeStartedWithName(pid, "obsolete", "anything") {
			t.Error("a process of another birth was read")
		}
	}
}
