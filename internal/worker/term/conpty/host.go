package conpty

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

const (
	defaultCols = 120
	defaultRows = 30

	// optionTask is the session option the Worker stores the task in.
	optionTask = "@minion_task"

	maxOptions   = 32
	maxOptionLen = 4096
	pasteChunk   = 4096

	bracketStart = "\x1b[200~"
	bracketEnd   = "\x1b[201~"
)

// Host implements term.SessionHost over pseudo consoles (AD-13). Each session
// has one pane that lives inside the Worker process, so a Worker restart ends
// every session; nothing is persisted.
type Host struct {
	start starter
	look  func(string) (string, error)
	now   func() time.Time

	// Drain timing after a program exits: the host waits until the output has
	// been quiet for drainQuiet, at most drainMax, before it closes the pseudo
	// console, so the last output is not cut off.
	drainQuiet, drainMax, drainPoll time.Duration

	mu       sync.Mutex
	sessions map[string]*session
	order    []string
	nextPane int
	nextName int
	closed   bool
}

var _ term.SessionHost = (*Host)(nil)

func newHost(start starter, look func(string) (string, error), now func() time.Time) *Host {
	return &Host{
		start: start, look: look, now: now,
		drainQuiet: 150 * time.Millisecond, drainMax: 2 * time.Second, drainPoll: 25 * time.Millisecond,
		sessions: map[string]*session{},
	}
}

// session is one session with its single pane.
type session struct {
	name    string
	created time.Time
	paneID  string
	pid     int
	command string
	p       proc

	wmu     sync.Mutex // serialises writes to the program with their KEEP re-check
	replies chan []byte
	stop    chan struct{} // closed when the reply writer must end

	closeOnce sync.Once // the pseudo console is closed exactly once
	closeErr  error

	mu       sync.Mutex // guards the fields below
	vt       *vt
	exited   bool
	lastOut  time.Time
	options  map[string]string
	readDone chan struct{}
}

// closeProc closes the pseudo console exactly once, whoever asks first (the
// exit watcher or Host.Close). Closing a pseudo console twice is a
// use-after-free on Windows. Later calls return the first result.
func (s *session) closeProc() error {
	s.closeOnce.Do(func() { s.closeErr = s.p.Close() })
	return s.closeErr
}

// ref returns the pane reference as the KEEP re-check sees it.
func (s *session) ref() term.PaneRef {
	return term.PaneRef{ID: s.paneID, PID: s.pid, Command: s.command}
}

// ListSessions returns the sessions in creation order.
func (h *Host) ListSessions(ctx context.Context) ([]term.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	list := make([]*session, 0, len(h.order))
	for _, n := range h.order {
		list = append(list, h.sessions[n])
	}
	h.mu.Unlock()
	out := make([]term.Session, 0, len(list))
	for _, s := range list {
		out = append(out, s.info())
	}
	return out, nil
}

func (s *session) info() term.Session {
	s.mu.Lock()
	task := s.options[optionTask]
	s.mu.Unlock()
	return term.Session{
		Name: s.name, Created: s.created, Windows: 1, Attached: false, Task: task,
		Panes: []term.Pane{{Ref: s.ref(), Window: 0, Index: 0, Active: true}},
	}
}

// ResolvePane resolves a target to the one pane of its session. An explicit
// pane other than window 0, pane 0 does not exist.
func (h *Host) ResolvePane(ctx context.Context, t ops.Target) (term.PaneRef, error) {
	if err := ctx.Err(); err != nil {
		return term.PaneRef{}, err
	}
	switch t.Pane {
	case "", "0.0", "0":
	default:
		return term.PaneRef{}, fmt.Errorf("pane %q of session %q: %w", t.Pane, t.SessionName, term.ErrNotFound)
	}
	h.mu.Lock()
	s, ok := h.sessions[t.SessionName]
	h.mu.Unlock()
	if !ok {
		return term.PaneRef{}, fmt.Errorf("session %q: %w", t.SessionName, term.ErrNotFound)
	}
	return s.ref(), nil
}

// byPane finds the session that owns a pane id.
func (h *Host) byPane(id string) (*session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		if s.paneID == id {
			return s, nil
		}
	}
	return nil, fmt.Errorf("pane %q: %w", id, term.ErrNotFound)
}

// Snapshot returns the visible screen, sanitised.
func (h *Host) Snapshot(ctx context.Context, pane term.PaneRef) (ops.Screen, error) {
	if err := ctx.Err(); err != nil {
		return ops.Screen{}, err
	}
	s, err := h.byPane(pane.ID)
	if err != nil {
		return ops.Screen{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	scr, complete := s.vt.snapshotChecked()
	if !complete {
		return scr, fmt.Errorf("pane %s: the visible screen exceeds the limits of the screen builder: %w", pane.ID, term.ErrIncomplete)
	}
	return scr, nil
}

// Capture returns the last lines lines including the scrollback, sanitised.
func (h *Host) Capture(ctx context.Context, pane term.PaneRef, lines int) (ops.Screen, error) {
	if err := ctx.Err(); err != nil {
		return ops.Screen{}, err
	}
	s, err := h.byPane(pane.ID)
	if err != nil {
		return ops.Screen{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	scr, complete := s.vt.captureChecked(lines)
	if !complete {
		return ops.Screen{}, fmt.Errorf("pane %s: the capture exceeds the limits of the screen builder: %w", pane.ID, term.ErrIncomplete)
	}
	return scr, nil
}

// keep is the KEEP re-check: the pane id, the process id and the command must
// still be the expected ones, and the process must still run. The caller holds
// s.wmu.
func (s *session) keep(expect term.PaneRef) error {
	s.mu.Lock()
	exited := s.exited
	s.mu.Unlock()
	if exited {
		return fmt.Errorf("process %d of %s has exited: %w", s.pid, s.name, term.ErrPaneChanged)
	}
	if err := term.CheckExpect(s.ref(), expect); err != nil {
		return fmt.Errorf("%s: %w", s.name, err)
	}
	return nil
}

// bracketedPaste reports whether the program asked for bracketed paste.
func (s *session) bracketedPaste() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vt.bracketed
}

// PasteText writes text to the pane as typed input, verbatim.
//
// It is the ConPTY side of the one paste contract of term.SessionHost (the
// tmux adapter documents the same): the text is validated first with the
// shared rule term.CheckPasteText, and a text that does not qualify returns an
// error matching ops.ErrInvalidArgument with nothing sent. It must be valid
// UTF-8, non-empty, at most term.MaxPasteBytes, and hold no control character
// other than TAB and LF: a CR would submit, an ESC could end the bracketed
// paste early or start a control sequence, and NUL, DEL and the C1 range are
// input. A paste never sends those, whether it arrives through the op server,
// the local socket or a direct call of the port; intentional control input is
// SendKeys with a key name (Enter, Escape, C-c).
//
// The bytes are written unchanged. TAB and LF are not rewritten (an earlier
// version turned every LF into CR, which is the Enter key: a multi-line text
// submitted its first lines), and the adapter adds no Enter. When the program
// enabled bracketed paste (DECSET 2004) the text is framed by the paste markers
// and the program can tell a paste from typing; otherwise the bytes arrive
// unwrapped, and a program that reads a received LF as a line submission may
// act on it. Console programs differ here, and the adapter cannot know which
// one is in the pane. The caller who must not submit sends one line.
//
// The text goes out in chunks. A cancelled context stops between chunks, and
// the closing marker of an open bracket is always written, so the program is
// not left in paste mode. After the first byte went out, any failure is a plain
// error and not "not sent": the caller maps it to unknown (AD-15). The KEEP
// re-check (pane id, process id and command, process still running) runs under
// the write lock immediately before the first byte.
func (h *Host) PasteText(ctx context.Context, pane term.PaneRef, text string, expect term.PaneRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := term.CheckPasteText(text); err != nil {
		return err
	}
	s, err := h.byPane(pane.ID)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	// The request may have been cancelled while it waited for the lock.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.keep(expect); err != nil {
		return err
	}
	bracketed := s.bracketedPaste()
	if bracketed {
		if _, err := s.p.Write([]byte(bracketStart)); err != nil {
			return fmt.Errorf("write to %s: %w", s.name, err)
		}
	}
	// endPaste closes an open bracket, also when the paste stops early: a
	// program left in paste mode would read later keys as pasted text.
	endPaste := func() error {
		if !bracketed {
			return nil
		}
		if _, err := s.p.Write([]byte(bracketEnd)); err != nil {
			return fmt.Errorf("write to %s: %w", s.name, err)
		}
		return nil
	}
	for b := []byte(text); len(b) > 0; {
		n := min(len(b), pasteChunk)
		if _, err := s.p.Write(b[:n]); err != nil {
			_ = endPaste()
			return fmt.Errorf("write to %s: %w", s.name, err)
		}
		b = b[n:]
		if len(b) > 0 {
			if err := ctx.Err(); err != nil {
				_ = endPaste()
				return err
			}
		}
	}
	return endPaste()
}

var _ term.VerbatimPaster = (*Host)(nil)

// VerbatimPaste reports that PasteText is the verbatim paste
// (term.VerbatimPaster): the Worker announces it to the Hub from this.
func (h *Host) VerbatimPaste() bool { return true }

// SendKeys sends tmux-named keys one by one, re-checking expect before each. A
// key name the host cannot encode is rejected before anything is sent.
func (h *Host) SendKeys(ctx context.Context, pane term.PaneRef, keys []string, expect term.PaneRef) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s, err := h.byPane(pane.ID)
	if err != nil {
		return 0, err
	}
	encoded := make([][]byte, len(keys))
	for i, k := range keys {
		if encoded[i], err = encodeKey(k); err != nil {
			return 0, err
		}
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	// The request may have been cancelled while it waited for the lock.
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	for i, b := range encoded {
		if err := s.keep(expect); err != nil {
			return i, err
		}
		if _, err := s.p.Write(b); err != nil {
			return i, fmt.Errorf("write to %s: %w", s.name, err)
		}
		if i+1 < len(encoded) {
			if err := ctx.Err(); err != nil {
				return i + 1, err
			}
		}
	}
	return len(encoded), nil
}

// CreateSession starts argv in a new pseudo console. An empty argv starts pwsh
// when it is installed and cmd otherwise. An empty name becomes the next free
// minion-N; the Worker normally passes its own persisted name.
func (h *Host) CreateSession(ctx context.Context, name, cwd string, argv []string) (term.Session, error) {
	if err := ctx.Err(); err != nil {
		return term.Session{}, err
	}
	spec := startSpec{dir: cwd, cols: defaultCols, rows: defaultRows}
	if len(argv) == 0 {
		spec.path = h.defaultShell()
	} else {
		spec.path, spec.args = argv[0], argv[1:]
	}
	// A bare program name is resolved through PATH here. The pseudo console
	// library would look it up relative to the working directory instead,
	// which fails or runs a program planted there.
	if !strings.ContainsAny(spec.path, `\/`) {
		p, err := h.look(spec.path)
		if err != nil || p == "" {
			return term.Session{}, fmt.Errorf("conpty: program %q not found on PATH: %w", spec.path, errors.Join(err, exec.ErrNotFound))
		}
		spec.path = p
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return term.Session{}, errors.New("conpty: host is closed")
	}
	if name == "" {
		for {
			h.nextName++
			name = "minion-" + strconv.Itoa(h.nextName)
			if _, taken := h.sessions[name]; !taken {
				break
			}
		}
	} else if _, taken := h.sessions[name]; taken {
		return term.Session{}, fmt.Errorf("conpty: session %q already exists", name)
	}

	s := &session{
		name: name, created: h.now(), command: commandName(spec.path),
		options: map[string]string{}, replies: make(chan []byte, 16), stop: make(chan struct{}),
		readDone: make(chan struct{}),
	}
	s.vt = newVT(spec.cols, spec.rows, s.queueReply)
	p, err := h.start(spec)
	if err != nil {
		return term.Session{}, fmt.Errorf("conpty: start %q: %w", spec.path, err)
	}
	h.nextPane++
	s.p, s.pid, s.paneID = p, p.Pid(), "%"+strconv.Itoa(h.nextPane)
	s.lastOut = h.now()
	h.sessions[name] = s
	h.order = append(h.order, name)
	go s.pump(h.now)
	go s.writeReplies()
	go s.watch(h)
	return s.info(), nil
}

func (h *Host) defaultShell() string {
	if p, err := h.look("pwsh.exe"); err == nil && p != "" {
		return p
	}
	return "cmd.exe"
}

// commandName is the command the KEEP re-check compares: the program's base
// name without directory and extension.
func commandName(path string) string {
	if i := strings.LastIndexAny(path, `\/`); i >= 0 {
		path = path[i+1:]
	}
	lower := strings.ToLower(path)
	for _, ext := range [...]string{".exe", ".com", ".bat", ".cmd"} {
		if strings.HasSuffix(lower, ext) {
			return path[:len(path)-len(ext)]
		}
	}
	return path
}

// SetOption stores a session option. The only option the host interprets is
// @minion_task, reported as Session.Task.
func (h *Host) SetOption(ctx context.Context, name, key, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	s, ok := h.sessions[name]
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("session %q: %w", name, term.ErrNotFound)
	}
	if len(value) > maxOptionLen {
		return fmt.Errorf("conpty: option %q is longer than %d bytes", key, maxOptionLen)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.options[key]; !ok && len(s.options) >= maxOptions {
		return fmt.Errorf("conpty: more than %d options", maxOptions)
	}
	s.options[key] = value
	return nil
}

// Close ends every program and its pseudo console. The host never calls it on
// its own: it is for the Worker's shutdown.
func (h *Host) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	list := make([]*session, 0, len(h.order))
	for _, n := range h.order {
		list = append(list, h.sessions[n])
	}
	h.mu.Unlock()
	var errs []error
	for _, s := range list {
		_ = s.p.Kill()
		errs = append(errs, s.closeProc())
	}
	return errors.Join(errs...)
}

// --- per-session goroutines --------------------------------------------------

// pump copies the pseudo console output into the screen buffer until the
// pseudo console is closed.
func (s *session) pump(now func() time.Time) {
	defer close(s.readDone)
	buf := make([]byte, 8192)
	for {
		n, err := s.p.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.vt.feed(buf[:n])
			s.lastOut = now()
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// watch waits for the program to exit, lets the last output arrive and then
// closes the pseudo console. The session stays, with its last screen.
func (s *session) watch(h *Host) {
	_ = s.p.Wait()
	s.mu.Lock()
	s.exited = true
	s.mu.Unlock()

	deadline := h.now().Add(h.drainMax)
	for {
		select {
		case <-s.readDone:
		case <-time.After(h.drainPoll):
			s.mu.Lock()
			quiet := h.now().Sub(s.lastOut)
			s.mu.Unlock()
			if quiet < h.drainQuiet && h.now().Before(deadline) {
				continue
			}
		}
		break
	}
	_ = s.closeProc()
	select {
	case <-s.readDone:
	case <-time.After(h.drainMax):
	}
	close(s.stop)
}

// queueReply hands a terminal reply (cursor position, device attributes) to
// the writer goroutine without blocking the output pump. It runs with s.mu
// held, so it must not take any lock.
func (s *session) queueReply(b []byte) {
	select {
	case s.replies <- bytes.Clone(b):
	default: // a program that floods queries gets no answers
	}
}

func (s *session) writeReplies() {
	for {
		select {
		case <-s.stop:
			return
		case r := <-s.replies:
			s.wmu.Lock()
			s.mu.Lock()
			exited := s.exited
			s.mu.Unlock()
			if !exited {
				_, _ = s.p.Write(r)
			}
			s.wmu.Unlock()
		}
	}
}
