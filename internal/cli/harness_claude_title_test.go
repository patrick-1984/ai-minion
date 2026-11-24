package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

const titleSession = "0f6a3c2e-1111-4222-8333-944455556666"

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "projects", "-home-user-app")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, titleSession+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func titleLine(kind, field, title, session string) string {
	b, _ := json.Marshal(map[string]string{"type": kind, field: title, "sessionId": session})
	return string(b)
}

// A96: only custom-title entries (/rename, --name) are read; AI titles,
// prompts and other entries are not, the latest custom title wins and the read
// is bounded to the transcript tail.
func TestClaudeCustomTitleReadsOnlyCustomTitles(t *testing.T) {
	prompt := `{"type":"user","message":{"role":"user","content":"\"custom-title\" secret prompt"},"sessionId":"` + titleSession + `"}`
	path := writeTranscript(t,
		titleLine("custom-title", "customTitle", "first", titleSession),
		prompt,
		titleLine("custom-title", "customTitle", "second", titleSession),
		titleLine("ai-title", "aiTitle", "summary of the prompts", titleSession),
		titleLine("custom-title", "customTitle", "from another session", "other"),
		`{"type":"custom-title","customTitle":`, // a torn last write
	)
	if got, ok := claudeCustomTitle(path, titleSession); !ok || got != "second" {
		t.Fatalf("custom title = %q, %v", got, ok)
	}
	if _, ok := claudeCustomTitle(writeTranscript(t, prompt, titleLine("ai-title", "aiTitle", "summary", titleSession)), titleSession); ok {
		t.Fatal("an AI title or a prompt was read as a name")
	}

	// Bounded: a title further back than the tail is not seen, one inside is.
	filler := `{"type":"assistant","x":"` + strings.Repeat("a", 1024) + `"}`
	var lines []string
	lines = append(lines, titleLine("custom-title", "customTitle", "too old", titleSession))
	for range maxClaudeTitleTail/len(filler) + 2 {
		lines = append(lines, filler)
	}
	if _, ok := claudeCustomTitle(writeTranscript(t, lines...), titleSession); ok {
		t.Fatal("read beyond the bounded tail")
	}
	lines = append(lines, titleLine("custom-title", "customTitle", "recent", titleSession))
	if got, ok := claudeCustomTitle(writeTranscript(t, lines...), titleSession); !ok || got != "recent" {
		t.Fatalf("recent title in a long transcript = %q, %v", got, ok)
	}

	// Only a transcript path of the expected shape is opened.
	for _, bad := range []string{
		"relative/projects/p/" + titleSession + ".jsonl",
		filepath.Dir(path) + "/../" + filepath.Base(filepath.Dir(path)) + "/" + titleSession + ".jsonl", // not clean
		filepath.Join(filepath.Dir(path), "other.jsonl"),
		filepath.Join(t.TempDir(), "elsewhere", "p", titleSession+".jsonl"),
		filepath.Dir(path),
	} {
		if _, ok := claudeCustomTitle(bad, titleSession); ok {
			t.Errorf("opened %s", bad)
		}
	}
	if _, ok := claudeCustomTitle(path, "../x"); ok {
		t.Error("a session id with a path separator was accepted")
	}
}

type fakeTitleStore struct {
	stored, checked string
	recorded        []string
	marks           int
}

func (f *fakeTitleStore) StoredHarnessTitle(context.Context, string, string, string, int64, string) (string, string, error) {
	return f.stored, f.checked, nil
}

func (f *fakeTitleStore) RecordHarnessTitle(_ context.Context, _ string, _ store.HarnessEvent, title string) error {
	f.recorded = append(f.recorded, title)
	f.stored, f.checked = title, ""
	return nil
}

func (f *fakeTitleStore) MarkHarnessTitleChecked(_ context.Context, _ string, _ store.HarnessEvent, digest string) error {
	f.marks++
	f.checked = digest
	return nil
}

// A96: the status line's session_name is kept only when the transcript
// confirms it as a custom title, sanitized; an AI title is never kept.
func TestRecordClaudeTitleKeepsOnlyConfirmedNames(t *testing.T) {
	ctx := t.Context()
	e := store.HarnessEvent{Session: "work", Pane: "%1", Created: 1, Harness: "claude", NativeID: strings.Repeat("a", 64)}
	statusLine := func(name *string, transcript string) []byte {
		m := map[string]any{"session_id": titleSession, "transcript_path": transcript}
		if name != nil {
			m["session_name"] = *name
		}
		b, _ := json.Marshal(m)
		return b
	}
	str := func(s string) *string { return &s }
	renamed := writeTranscript(t, titleLine("ai-title", "aiTitle", "AI summary", titleSession), titleLine("custom-title", "customTitle", "release\x1b[2J  prep", titleSession))
	aiOnly := writeTranscript(t, titleLine("ai-title", "aiTitle", "AI summary", titleSession))

	st := &fakeTitleStore{}
	recordClaudeTitle(ctx, st, "g", e, statusLine(str("AI summary"), aiOnly))
	if len(st.recorded) != 0 || st.marks != 1 || strings.Contains(st.checked, "AI") {
		t.Fatalf("AI title kept: %q (marks %d, checked %q)", st.recorded, st.marks, st.checked)
	}
	// The same unconfirmed name is not looked up again on the next redraw.
	recordClaudeTitle(ctx, st, "g", e, statusLine(str("AI summary"), aiOnly))
	if st.marks != 1 {
		t.Fatalf("unconfirmed name checked again: %d", st.marks)
	}
	recordClaudeTitle(ctx, st, "g", e, statusLine(str("release\x1b[2J  prep"), renamed))
	if st.stored != "release[2J prep" {
		t.Fatalf("stored = %q", st.stored)
	}
	// The same name again does not read the transcript or write.
	recordClaudeTitle(ctx, st, "g", e, statusLine(str("release\x1b[2J prep"), "/nonexistent"))
	if len(st.recorded) != 1 {
		t.Fatalf("writes = %q", st.recorded)
	}
	// A name the tail cannot confirm keeps the known one.
	recordClaudeTitle(ctx, st, "g", e, statusLine(str("AI summary"), aiOnly))
	if st.stored != "release[2J prep" {
		t.Fatalf("unconfirmed name replaced the known one: %q", st.stored)
	}
	// No session_name: the session has no name (any more).
	recordClaudeTitle(ctx, st, "g", e, statusLine(nil, renamed))
	if st.stored != "" || len(st.recorded) != 2 {
		t.Fatalf("absent name kept %q (%q)", st.stored, st.recorded)
	}
}
