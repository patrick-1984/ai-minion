package tmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// paneFormat returns the tmux format of one pane row.
//
// tmux prints format values raw: it does not escape a tab, a newline or a
// backslash, so #{@minion_task}, #{@minion_probe}, #{pane_current_command} and #{session_name}
// can contain any byte a user, a process argv or a session name carries. A
// fixed separator therefore cannot frame a row. The format instead uses a field
// separator and a row terminator that each contain a 128-bit random nonce,
// generated per call: content written before the call cannot contain them, so
// it can neither split nor forge a row, and every value stays lossless.
const paneFormatFields = "#{session_created}%[1]s#{session_attached}%[1]s#{session_windows}%[1]s" +
	"#{window_index}%[1]s#{window_active}%[1]s#{pane_index}%[1]s#{pane_active}%[1]s" +
	"#{pane_id}%[1]s#{pane_pid}%[1]s" +
	"#{@minion_task}%[1]s#{@minion_probe}%[1]s#{pane_current_command}%[1]s#{session_name}%[2]s"

const paneFields = 13

// paneCodec holds the delimiters of one list-panes invocation.
type paneCodec struct {
	fs string // field separator
	rs string // row terminator; tmux appends its own newline after it
}

// newPaneCodec returns a codec with fresh random delimiters. The delimiters use
// only letters, digits and underscores, which tmux's format parser leaves
// alone.
func newPaneCodec() (paneCodec, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return paneCodec{}, fmt.Errorf("tmux: no randomness for list-panes framing: %w", err)
	}
	n := hex.EncodeToString(b[:16])
	return paneCodec{fs: "_MF" + n + "_", rs: "_MR" + hex.EncodeToString(b[16:]) + "_"}, nil
}

// format returns the -F argument of list-panes.
func (c paneCodec) format() string { return fmt.Sprintf(paneFormatFields, c.fs, c.rs) }

// ListSessions returns every session of the tmux server with its panes.
// Sessions are returned exactly as tmux names them, byte for byte: adopted
// sessions are never renamed (R86), and tmux's own stored form of a name (it
// vis-escapes names when it creates a session) is the name that has-session
// and every later command match, so it is not decoded. A server that is not running has no sessions: the result is
// empty and the error nil.
func (h *Host) ListSessions(ctx context.Context) ([]term.Session, error) {
	// A server that another process started on an old tmux may have a
	// group-writable socket; the listing runs every poll, so it is fixed soon.
	h.tightenSocket()
	sessions, err := h.listPanes(ctx)
	if errors.Is(err, term.ErrNotFound) {
		return nil, nil
	}
	return sessions, err
}

// listPanes runs one list-panes -a and groups the rows into sessions, in the
// order tmux reports them.
func (h *Host) listPanes(ctx context.Context) ([]term.Session, error) {
	return h.listPaneMetadata(ctx, false)
}

// ListUsageSessions asks only for terminal host metadata. It substitutes a
// native session ID and folder for the task and probe options on this call.
func (h *Host) ListUsageSessions(ctx context.Context) ([]term.Session, error) {
	rows, err := h.listPaneMetadata(ctx, true)
	if errors.Is(err, term.ErrNotFound) {
		return nil, nil
	}
	return rows, err
}

func (h *Host) listPaneMetadata(ctx context.Context, usage bool) ([]term.Session, error) {
	codec, err := newPaneCodec()
	if err != nil {
		return nil, err
	}
	format := codec.format()
	if usage {
		format = strings.NewReplacer("#{@minion_task}", "#{session_id}", "#{@minion_probe}", "#{pane_current_path}").Replace(format)
	}
	out, err := h.run(ctx, "list-panes", "-a", "-F", format)
	if err != nil {
		h.debugList(0, nil, err)
		return nil, err
	}
	sessions, err := codec.parseMetadata(string(out), usage)
	if err != nil || !h.psmux || usage {
		h.debugList(len(sessions), nil, err)
		return sessions, err
	}
	// psmux numbers panes per session: address each one through its session.
	// A session whose name psmux cannot target literally is left out: a target
	// it misreads falls back to the last-used session (fail closed).
	kept := sessions[:0]
	var dropped []string
	for i := range sessions {
		if !PsmuxNameOK(sessions[i].Name) {
			dropped = append(dropped, sessions[i].Name)
			continue
		}
		for j := range sessions[i].Panes {
			p := &sessions[i].Panes[j]
			p.Ref.Target = psmuxPaneTarget(sessions[i].Name, p.Window, p.Index)
		}
		kept = append(kept, sessions[i])
	}
	h.debugList(len(kept), dropped, nil)
	return kept, nil
}

// debugList writes one debug line about a session listing: the command, the
// outcome, the number of sessions, and for psmux the sessions left out because
// psmux cannot address their names (at most 5 names, cut). A listing that has
// the same result as the previous one is not written again (the event stream
// lists every second).
func (h *Host) debugList(n int, dropped []string, err error) {
	names := make([]string, 0, min(len(dropped), 5))
	for _, d := range dropped[:min(len(dropped), 5)] {
		names = append(names, strconv.Quote(truncate(d, 40)))
	}
	e := errText(err)
	var te *tmuxError
	if errors.As(err, &te) {
		e = truncate(e, 300)
	}
	key := fmt.Sprintf("%d|%d|%s|%s", n, len(dropped), strings.Join(names, ","), e)
	h.listDedup.Debug(h.logger(), key, "terminal host listing", "command", "list-panes -a", "binary", h.bin, "psmux", h.psmux,
		"ok", err == nil, "sessions", n, "dropped_unaddressable", len(dropped), "dropped_names", strings.Join(names, ","),
		"error", e)
}

// PsmuxNameOK reports whether psmux 3.4 reads "=name:" as exactly that
// session: not a leading '%', '@', '$' or '=' (pane, window, session-id and
// exact prefixes) and none of the separators and quotes ':', '.', '\”, '"',
// '#' and ';'.
func PsmuxNameOK(name string) bool {
	if name == "" || strings.ContainsAny(name[:1], "%@$=") || strings.ContainsAny(name, ":.'\"#;") || hasControl(name) {
		return false
	}
	return true
}

// psmuxPaneTarget addresses a pane of psmux by session, window and pane index.
func psmuxPaneTarget(session string, window, index int) string {
	return exactSession(session) + strconv.Itoa(window) + "." + strconv.Itoa(index)
}

// parse parses the output of list-panes -a -F c.format(). Every row ends with
// the row terminator and tmux's newline; a newline inside a field value is data.
func (c paneCodec) parse(out string) ([]term.Session, error) {
	return c.parseMetadata(out, false)
}

func (c paneCodec) parseMetadata(out string, usage bool) ([]term.Session, error) {
	var sessions []term.Session
	index := map[string]int{}
	rows := strings.Split(out, c.rs+"\n")
	if last := rows[len(rows)-1]; last != "" {
		return nil, errors.New("tmux: list-panes output ends in an unterminated row")
	}
	rows = rows[:len(rows)-1]
	for n, row := range rows {
		f := strings.Split(row, c.fs)
		if len(f) != paneFields {
			return nil, fmt.Errorf("tmux: list-panes row %d has %d fields, want %d", n+1, len(f), paneFields)
		}
		created, e1 := strconv.ParseInt(f[0], 10, 64)
		attached, e2 := strconv.Atoi(f[1])
		windows, e3 := strconv.Atoi(f[2])
		winIdx, e4 := strconv.Atoi(f[3])
		paneIdx, e5 := strconv.Atoi(f[5])
		pid, e6 := parsePID(f[8])
		if err := errors.Join(e1, e2, e3, e4, e5, e6); err != nil {
			return nil, fmt.Errorf("tmux: list-panes row %d: %w", n+1, err)
		}
		name := f[12]
		task, probe := f[9], f[10]
		var id, folder string
		if usage {
			id, folder = task, probe
			task, probe = "", ""
		}
		i, ok := index[name]
		if !ok {
			i = len(sessions)
			index[name] = i
			sessions = append(sessions, term.Session{
				Name:     name,
				Created:  time.Unix(created, 0).UTC(),
				Windows:  windows,
				Attached: attached > 0,
				Task:     task, HostID: id,
				Probe: probe,
			})
		}
		sessions[i].Panes = append(sessions[i].Panes, term.Pane{
			Folder: folder,
			Ref:    term.PaneRef{ID: f[7], PID: pid, Command: f[11]},
			Window: winIdx,
			Index:  paneIdx,
			Active: f[4] == "1" && f[6] == "1",
		})
	}
	return sessions, nil
}

// parsePID parses a pane_pid. tmux 3.8 prints an empty value for a dead pane
// (remain-on-exit), whose process is gone; that reads as pid 0. Older versions
// print the pid of the ended process.
func parsePID(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	return strconv.Atoi(v)
}
