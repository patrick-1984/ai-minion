package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// fakeTitleStore keeps one session's row with the semantics of the Worker
// store (store.ObserveHarnessTitle and the others; the store tests cover the
// real one).
type fakeTitleStore struct {
	title, verified, checked string
	scanned                  int64
	at                       time.Time
	confirmed                []string
	marks                    int
}

func (f *fakeTitleStore) ObserveHarnessTitle(_ context.Context, _ string, e store.HarnessEvent, want string) (store.HarnessTitleObservation, error) {
	if e.At.Before(f.at) {
		return store.HarnessTitleObservation{}, nil
	}
	f.at, f.title = e.At, ""
	if want != "" && want == f.verified {
		f.title = want
	}
	return store.HarnessTitleObservation{Verify: want != "" && f.title == "", Checked: f.checked, Scanned: f.scanned}, nil
}

func (f *fakeTitleStore) ConfirmHarnessTitle(_ context.Context, _ string, e store.HarnessEvent, title string) error {
	f.confirmed = append(f.confirmed, title)
	f.verified = title
	if e.At.Equal(f.at) {
		f.title = title
	}
	return nil
}

func (f *fakeTitleStore) MarkHarnessTitleChecked(_ context.Context, _ string, e store.HarnessEvent, digest string, scanned int64) error {
	if e.At.Equal(f.at) || (digest == f.checked && scanned > f.scanned) {
		f.marks++
		f.checked, f.scanned = digest, scanned
	}
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

// observedAt is titleEvent observed at the n-th tick.
func observedAt(n int) store.HarnessEvent {
	e := titleEvent
	e.At = time.Unix(1_700_000_000, 0).Add(time.Duration(n) * time.Millisecond)
	return e
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
