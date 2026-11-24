package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

const (
	// maxClaudeTitleScan bounds the transcript bytes one status-line callback
	// searches; a longer transcript is searched over the next callbacks, from
	// where the last one stopped.
	maxClaudeTitleScan = 32 << 20
	// maxClaudeTitleLine bounds the transcript line that can hold a /rename
	// record: longer lines (prompts, tool results, API requests) are skipped
	// without being decoded or kept.
	maxClaudeTitleLine = 64 << 10
	// maxClaudeCmdline bounds the process arguments read for --name.
	maxClaudeCmdline = 256 << 10
)

// claudeTitleStore is the part of the Worker store the session name needs.
type claudeTitleStore interface {
	StoredHarnessTitle(ctx context.Context, session, pane, harness string, created int64, native string) (store.HarnessTitleCheck, error)
	RecordHarnessTitle(ctx context.Context, grant string, e store.HarnessEvent, title string) error
	MarkHarnessTitleChecked(ctx context.Context, grant string, e store.HarnessEvent, digest string, scanned int64) error
}

// recordClaudeTitle keeps the name the user gave the conversation. The status
// line's session_name is not enough: it falls back to a title generated from
// the prompts, and an unnamed /branch or a prompt hook writes a custom title
// derived from submitted text, none of them marked as such. So a name is kept
// only on positive evidence that the user typed it:
//
//   - the claude process was started with --name <name> (or -n), or
//   - the transcript holds the record of the user's own /rename <name> command
//     (a local_command system entry whose argument is the name).
//
// Otherwise the session shows no name. Only those records are decoded; the
// transcript path, the arguments and every other line are discarded. The
// transcript is opened only beneath the verified Claude profile directory.
// Absent session_name means no name.
func recordClaudeTitle(ctx context.Context, st claudeTitleStore, grant, profile string, e store.HarnessEvent, raw []byte) {
	var n struct {
		Session    string  `json:"session_id"`
		Name       *string `json:"session_name"`
		Transcript string  `json:"transcript_path"`
	}
	if json.Unmarshal(raw, &n) != nil || n.Session == "" {
		return
	}
	kept, err := st.StoredHarnessTitle(ctx, e.Session, e.Pane, e.Harness, e.Created, e.NativeID)
	if err != nil {
		return
	}
	want := ""
	if n.Name != nil {
		want = ops.SanitizeHarnessName(*n.Name)
	}
	if want == kept.Title {
		return
	}
	if want == "" {
		_ = st.RecordHarnessTitle(ctx, grant, e, "")
		return
	}
	if claudeStartedWithName(e.ProcessPID, e.ProcessBirth, want) {
		_ = st.RecordHarnessTitle(ctx, grant, e, want)
		return
	}
	f, id, err := openClaudeTranscript(profile, n.Transcript, n.Session)
	if err != nil {
		return // not readable now: nothing is recorded, the next redraw tries again
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return
	}
	// The digest covers the name and the transcript file: the same name in
	// the same file is searched again only in what the file gained since.
	digest := ops.UsageID("claude-title-check", grant, n.Session, want, id)
	from := int64(0)
	if kept.Checked == digest && kept.Scanned <= info.Size() {
		from = kept.Scanned
		if from == info.Size() {
			return // nothing new since the last search
		}
	}
	found, scanned, err := claudeRenamed(ctx, f, from, info.Size(), n.Session, want)
	switch {
	case found:
		_ = st.RecordHarnessTitle(ctx, grant, e, want)
	case err == nil || scanned > from:
		_ = st.MarkHarnessTitleChecked(ctx, grant, e, digest, scanned)
	}
}

// claudeRenamed searches the transcript f from offset from up to size (at
// most [maxClaudeTitleScan] bytes, and only while ctx lasts) for the user's
// /rename of this session to want. It returns the offset after the last
// complete line searched: the next search starts there.
func claudeRenamed(ctx context.Context, f io.ReaderAt, from, size int64, session, want string) (bool, int64, error) {
	end := min(size, from+maxClaudeTitleScan)
	r := bufio.NewReaderSize(io.NewSectionReader(f, from, end-from), maxClaudeTitleLine)
	pos, skipping := from, false
	if from > 0 {
		// The last search stopped inside a long line: its rest is skipped.
		var prev [1]byte
		if _, err := f.ReadAt(prev[:], from-1); err != nil {
			return false, from, err
		}
		skipping = prev[0] != '\n'
	}
	for {
		if ctx.Err() != nil {
			return false, pos, ctx.Err()
		}
		line, err := r.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			skipping = true // a long line: never a /rename record
			pos += int64(len(line))
			continue
		case err != nil:
			// A last line without its newline is still being written (or
			// cut by the scan bound): it is searched again, whole, next time.
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return false, pos, err
		}
		pos += int64(len(line))
		if skipping {
			skipping = false
			continue
		}
		if claudeRenameRecord(line, session) == want {
			return true, pos, nil
		}
	}
}

// claudeRenameRecord returns the sanitized argument of a /rename command
// record of the session ("" when line is not one). Claude Code writes it as a
// system entry of subtype local_command when the user runs the command; a
// prompt that quotes the command is a user entry and does not count, and
// /rename without an argument (a generated name) has none.
func claudeRenameRecord(line []byte, session string) string {
	const head = "<command-name>/rename</command-name>"
	// Cheap prefilter; the markup may be JSON-escaped (\u003c), so it is
	// matched only after decoding.
	if !bytes.Contains(line, []byte(`"local_command"`)) || !bytes.Contains(line, []byte("/rename")) {
		return ""
	}
	var entry struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Content string `json:"content"`
		Session string `json:"sessionId"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Type != "system" || entry.Subtype != "local_command" || entry.Session != session {
		return ""
	}
	content := strings.TrimSpace(entry.Content)
	if !strings.HasPrefix(content, head) {
		return ""
	}
	_, args, ok := strings.Cut(content, "<command-args>")
	if !ok {
		return ""
	}
	args, _, ok = strings.Cut(args, "</command-args>")
	if !ok {
		return ""
	}
	return ops.SanitizeHarnessName(args)
}

// claudeStartedWithName reports whether the claude process pid (still the one
// born at birth) was started with --name want or -n want.
func claudeStartedWithName(pid int, birth, want string) bool {
	if pid <= 0 || birth == "" || agentdetect.ProcessBirth(pid) != birth {
		return false
	}
	f, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxClaudeCmdline))
	_ = f.Close()
	if err != nil || agentdetect.ProcessBirth(pid) != birth {
		return false
	}
	return claudeNameArg(strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")) == want
}

// claudeNameArg returns the sanitized value of the last --name / -n option of
// a claude argv ("" for none). Options end at "--".
func claudeNameArg(argv []string) string {
	name := ""
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--":
			return name
		case a == "--name" || a == "-n":
			if i+1 < len(argv) {
				i++
				name = ops.SanitizeHarnessName(argv[i])
			}
		case strings.HasPrefix(a, "--name="):
			name = ops.SanitizeHarnessName(strings.TrimPrefix(a, "--name="))
		}
	}
	return name
}
