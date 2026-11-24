package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// maxClaudeTitleTail bounds the transcript tail read for the session name.
// Claude Code appends a custom-title entry on /rename and repeats it as the
// transcript grows, so the latest one is normally near the end.
const maxClaudeTitleTail = 256 << 10

// claudeTitleStore is the part of the Worker store the session name needs.
type claudeTitleStore interface {
	StoredHarnessTitle(ctx context.Context, session, pane, harness string, created int64, native string) (string, string, error)
	RecordHarnessTitle(ctx context.Context, grant string, e store.HarnessEvent, title string) error
	MarkHarnessTitleChecked(ctx context.Context, grant string, e store.HarnessEvent, digest string) error
}

// recordClaudeTitle keeps the name the user gave the conversation (/rename or
// --name). The status line's session_name falls back to the AI-generated
// title, which summarizes the prompts, so it is kept only when the latest
// custom-title entry of the transcript says the same. Only those entries are
// decoded; the transcript path and every other line are discarded. Absent
// session_name means no name; a name the bounded tail cannot confirm keeps the
// name known before.
func recordClaudeTitle(ctx context.Context, st claudeTitleStore, grant string, e store.HarnessEvent, raw []byte) {
	var n struct {
		Session    string  `json:"session_id"`
		Name       *string `json:"session_name"`
		Transcript string  `json:"transcript_path"`
	}
	if json.Unmarshal(raw, &n) != nil || n.Session == "" {
		return
	}
	stored, checked, err := st.StoredHarnessTitle(ctx, e.Session, e.Pane, e.Harness, e.Created, e.NativeID)
	if err != nil {
		return
	}
	want := ""
	if n.Name != nil {
		want = ops.SanitizeHarnessName(*n.Name)
	}
	if want == stored {
		return
	}
	if want != "" {
		// The status line redraws every few seconds: a name already found
		// unconfirmed is not looked up again. Only its keyed digest is kept.
		digest := ops.UsageID("claude-title-check", grant, n.Session, want)
		if digest == checked {
			return
		}
		custom, found := claudeCustomTitle(n.Transcript, n.Session)
		if !found || ops.SanitizeHarnessName(custom) != want {
			_ = st.MarkHarnessTitleChecked(ctx, grant, e, digest)
			return
		}
	}
	_ = st.RecordHarnessTitle(ctx, grant, e, want)
}

// claudeCustomTitle returns the latest custom-title of a Claude Code
// transcript from its last [maxClaudeTitleTail] bytes. The path must have the
// transcript's shape: <profile>/projects/<project>/<session id>.jsonl.
func claudeCustomTitle(path, session string) (string, bool) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != session+".jsonl" ||
		strings.ContainsAny(session, `/\`) || filepath.Base(filepath.Dir(filepath.Dir(path))) != "projects" {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	offset := max(info.Size()-maxClaudeTitleTail, 0)
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
		return "", false
	}
	return lastCustomTitle(buf, offset > 0, session)
}

// lastCustomTitle scans JSON lines for the latest custom-title entry of the
// session. A partial first line (partial is true) is skipped.
func lastCustomTitle(buf []byte, partial bool, session string) (string, bool) {
	if partial {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return "", false
		}
		buf = buf[i+1:]
	}
	title, found := "", false
	for line := range bytes.SplitSeq(buf, []byte{'\n'}) {
		// Only entries that can be a custom title are decoded at all.
		if !bytes.Contains(line, []byte(`"custom-title"`)) {
			continue
		}
		var entry struct {
			Type    string  `json:"type"`
			Title   *string `json:"customTitle"`
			Session string  `json:"sessionId"`
		}
		if json.Unmarshal(line, &entry) != nil || entry.Type != "custom-title" || entry.Title == nil ||
			(entry.Session != "" && entry.Session != session) {
			continue
		}
		title, found = *entry.Title, true
	}
	return title, found
}
