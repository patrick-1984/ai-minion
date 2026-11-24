package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// A96: only the local_command record of the session's own /rename with an
// argument is evidence; a quoted command (a user entry), another session's
// record and a /rename without argument are not.
func TestClaudeRenameRecord(t *testing.T) {
	for line, want := range map[string]string{
		renameRecord("release prep", titleSession):    "release prep",
		renameRecord("  spaced\tname ", titleSession): "spaced name",
		renameRecord("", titleSession):                "",
		renameRecord("release prep", "other"):         "",
		promptLine("<command-name>/rename</command-name><command-args>x</command-args> \"local_command\""): "",
		customTitle("release prep"): "",
		`{"type":"system","subtype":"local_command","sessionId":"` + titleSession + `","content":"<command-name>/model</command-name><command-args>/rename x</command-args>"}`: "",
	} {
		if got := claudeRenameRecord([]byte(line), titleSession); got != want {
			t.Errorf("%s: %q, want %q", line, got, want)
		}
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
