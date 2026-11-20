// Package tray is the logic of the desktop tray (cmd/minion-tray): what the
// menu shows and which `minion` command each menu item runs. It imports no GUI
// library; the menu and the dialogs are behind the Dialogs and View
// interfaces, so the handlers are tested with a fake `minion` child process.
//
// Every action is a `minion` command; the tray reads nothing but the output of
// `minion worker status --json`.
package tray

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// CommandTimeout bounds every `minion` child process.
const CommandTimeout = 30 * time.Second

// Runner runs one `minion` command: argv without the program, optional stdin.
type Runner interface {
	Run(ctx context.Context, stdin string, args ...string) (stdout string, err error)
}

// CmdError is a failed command; Message is its standard error (or output).
type CmdError struct {
	Args    []string
	Message string
	Err     error
}

func (e *CmdError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("minion %s: %v", strings.Join(e.Args, " "), e.Err)
}

func (e *CmdError) Unwrap() error { return e.Err }

// ExecRunner runs the installed minion binary with a timeout.
type ExecRunner struct {
	Path    string
	Timeout time.Duration
}

// Run implements Runner.
func (r ExecRunner) Run(ctx context.Context, stdin string, args ...string) (string, error) {
	t := r.Timeout
	if t <= 0 {
		t = CommandTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Path, args...)
	cmd.WaitDelay = 2 * time.Second
	hideWindow(cmd)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			msg = fmt.Sprintf("minion %s did not finish in %s", strings.Join(args, " "), t)
		}
		return out.String(), &CmdError{Args: args, Message: msg, Err: err}
	}
	return out.String(), nil
}

// FindMinion returns the minion next to the tray binary, else the one on PATH.
func FindMinion() (string, error) {
	name := "minion"
	if runtime.GOOS == "windows" {
		name = "minion.exe"
	}
	if self, err := os.Executable(); err == nil {
		sib := filepath.Join(filepath.Dir(self), name)
		if st, err := os.Stat(sib); err == nil && !st.IsDir() {
			return sib, nil
		}
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("minion is not installed next to the tray or on PATH: install it first (see docs/guides/tray.md)")
	}
	return p, nil
}

// Status is the part of `minion worker status --json` the tray uses.
type Status struct {
	Name         string `json:"name"`
	Mode         string `json:"mode"`
	HubURL       string `json:"hub_url"`
	Paired       bool   `json:"paired"`
	Paused       bool   `json:"paused"`
	NewSessions  string `json:"new_sessions"`
	Sessions     *int   `json:"sessions"`
	SessionHost  string `json:"session_host"`
	HubWords     string `json:"hub_fingerprint_words"`
	HubCheck     string `json:"hub_check"`
	HubCheckText string `json:"hub_check_text"`
	Alarm        string `json:"alarm"`
	PairingState string `json:"pairing_state"`
	Approval     string `json:"approval"`
	Share        struct {
		Mode     string   `json:"mode"`
		Sessions []string `json:"sessions"`
	} `json:"share"`
	Connection struct {
		State string `json:"state"`
		Live  bool   `json:"live"`
	} `json:"connection"`
}

// Mismatch is true when the pinned Hub key was recorded as a mismatch or the
// running Worker refuses the key the Hub answers with.
func (s Status) Mismatch() bool {
	return s.HubCheck == "mismatch" || s.Alarm == "hub_identity_changed"
}

// Running is true when a running Worker answered.
func (s Status) Running() bool { return s.Connection.Live }

// SessionsCouldEnd is true when stopping or replacing the Worker may end open
// terminal sessions: a running Worker with a counted session, an unknown count
// or the ConPTY host (its sessions live inside the Worker); and, on Windows, a
// Worker that is set up but did not answer (a slow status pipe is not proof
// that nothing runs, and `worker down` signals the supervisor anyway). The
// status query can also miss a Worker of another profile or state directory,
// so on Windows an answer of "not running" never rules out a running Worker.
func (s Status) SessionsCouldEnd(goos string) bool {
	if !s.Running() {
		return goos == "windows"
	}
	return s.SessionHost == "conpty" || s.Sessions == nil || *s.Sessions > 0
}

// StatusLine is the first line of the menu.
func (s Status) StatusLine() string {
	switch {
	case !s.Running():
		if s.Paused {
			return "Worker stopped (paused)"
		}
		return "Worker stopped"
	case s.Paused:
		return "Worker running, paused"
	case s.Connection.State == "connected":
		return "Worker running, connected"
	default:
		return "Worker running, " + orDash(s.Connection.State)
	}
}

// HubLine is the Hub address line.
func (s Status) HubLine() string {
	if s.HubURL == "" || !s.Paired {
		return "Hub: not set up"
	}
	return "Hub: " + s.HubURL
}

// KeysLine is the six Hub key words, or a prominent MISMATCH.
func (s Status) KeysLine() string {
	if s.Mismatch() {
		return "!! MISMATCH: the Hub key changed. Do not trust this Hub !!"
	}
	if s.HubWords == "" {
		return "Hub key words: none"
	}
	return "Hub key: " + s.HubWords
}

func orDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

// Dialogs are the native dialogs. A cancelled dialog returns ok false.
type Dialogs interface {
	// Token asks for the join token (input hidden).
	Token() (token string, ok bool)
	// Confirm asks a yes/no question.
	Confirm(text string) bool
	// Pick shows a checklist; checked names are preselected.
	Pick(text string, all, checked []string) (chosen []string, ok bool)
	// Mode asks which sharing rule to apply: "keep", "all", "none", or, when
	// canSelect, "select" (narrow the shared sessions).
	Mode(text, current string, canSelect bool) (mode string, ok bool)
	Info(text string)
	Error(text string)
}

// View receives what the menu should show.
type View interface {
	Show(s Status, err error)
}

// Controller maps menu items to minion commands.
type Controller struct {
	R Runner
	D Dialogs
	V View

	// GOOS is the host system; empty means runtime.GOOS.
	GOOS string

	mu   sync.Mutex // one action at a time
	last Status
}

func (c *Controller) run(stdin string, args ...string) (string, error) {
	return c.R.Run(context.Background(), stdin, args...)
}

// Refresh runs `minion worker status --json` and updates the view.
func (c *Controller) Refresh() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh()
}

func (c *Controller) refresh() {
	out, err := c.run("", "worker", "status", "--json")
	var s Status
	if err == nil {
		if jerr := json.Unmarshal([]byte(out), &s); jerr != nil {
			err = fmt.Errorf("cannot read the status of minion: %w", jerr)
		}
	}
	if err == nil {
		c.last = s
	}
	c.V.Show(s, err)
}

// do runs one action, reports a failure in a dialog and refreshes.
func (c *Controller) do(stdin string, args ...string) bool {
	_, err := c.run(stdin, args...)
	if err != nil {
		c.D.Error(err.Error())
	}
	return err == nil
}

func (c *Controller) goos() string {
	if c.GOOS != "" {
		return c.GOOS
	}
	return runtime.GOOS
}

// confirmEnd asks before an action that can end open terminal sessions. It
// reads a fresh status first. False means: do not go on.
func (c *Controller) confirmEnd(what string) bool {
	c.refresh()
	if !c.last.SessionsCouldEnd(c.goos()) {
		return true
	}
	return c.D.Confirm(what + " can end the terminal sessions that are open. Go on?")
}

// Start is `minion worker start`.
func (c *Controller) Start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.do("", "worker", "start")
	c.refresh()
}

// Stop is `minion worker down`, confirmed when sessions could end.
func (c *Controller) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.confirmEnd("Stopping the Worker") {
		return
	}
	c.do("", "worker", "down")
	c.refresh()
}

// SetPaused runs `minion worker pause` (true) or `resume` (false). The target
// is the action of the menu item the person clicked, never derived from a
// fresh status: a stale "Pause" label can never reconnect the Worker. When the
// status already is the target, nothing runs.
func (c *Controller) SetPaused(pause bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh()
	if c.last.Paused == pause {
		return
	}
	if pause {
		c.do("", "worker", "pause")
	} else {
		c.do("", "worker", "resume")
	}
	c.refresh()
}

// Setup asks for a join token and runs `minion worker up --join -` with the
// token on stdin (never in the argument list).
func (c *Controller) Setup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	tok, ok := c.D.Token()
	tok = strings.TrimSpace(tok)
	if !ok || tok == "" {
		return
	}
	// `worker up` can replace a Worker that is running.
	if !c.confirmEnd("Setting up the Hub (it may restart the Worker)") {
		return
	}
	if c.do(tok+"\n", "worker", "up", "--join", "-") {
		c.D.Info("The Worker is set up. Check the Hub key words in the menu against the ones your Hub shows.")
	}
	c.refresh()
}

// ShareSessions is the sharing dialog. The current rule and the names come
// from `worker status --json` (share section) and the change is written with
// `worker config share all|none` or `share select -- <names>`. The saved names
// of a rule that is not "select" are never offered (they are not shared), and
// the checklist only narrows the shared sessions: the tray never sends a name
// the status did not give it. Windows shows no checklist at all (its status
// has no session names), only keep / all / none.
func (c *Controller) ShareSessions() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh()
	cur, names := c.last.Share.Mode, c.last.Share.Sessions
	canSelect := cur == "select" && len(names) > 0 && c.goos() != "windows"
	mode, ok := c.D.Mode("Choose what the Hub may show.", cur, canSelect)
	if !ok {
		return
	}
	args := []string{"worker", "config", "share"}
	switch mode {
	case "all", "none":
		args = append(args, mode)
	case "select":
		if !canSelect {
			return
		}
		chosen, ok := c.D.Pick("Uncheck the sessions the Hub may no longer show.", names, names)
		if !ok {
			return
		}
		if len(chosen) == 0 {
			args = append(args, "none")
		} else {
			args = append(args, "select", "--")
			args = append(args, chosen...)
		}
	default:
		return
	}
	if c.do("", args...) {
		c.refresh()
	}
}

// PickLabels returns one label per name for a checklist dialog, and a lookup
// from a label back to the name. Labels never start with a dash, so no dialog
// program reads a session name as an option; names can hold any character.
func PickLabels(names []string) (labels []string, name func(label string) (string, bool)) {
	byLabel := make(map[string]string, len(names))
	for i, n := range names {
		l := fmt.Sprintf("%d. %s", i+1, n)
		labels = append(labels, l)
		byLabel[l] = n
	}
	return labels, func(l string) (string, bool) { n, ok := byLabel[l]; return n, ok }
}

// SetNewSessions sets `config set allow_new_sessions` to target ("on" or
// "off"). The target comes from the checkbox the person clicked, never from a
// guess about the current state: if the state already is the target nothing is
// written, and the person confirms the target state first.
func (c *Controller) SetNewSessions(target string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if target != "on" && target != "off" {
		return
	}
	c.refresh()
	if c.last.NewSessions == target {
		return
	}
	q := "Block every new session on this machine? The Hub cannot lift the block."
	if target == "on" {
		q = "Allow new sessions on this machine again?"
	}
	if !c.D.Confirm(q) {
		c.refresh()
		return
	}
	c.do("", "worker", "config", "set", "allow_new_sessions", target)
	c.refresh()
}

// Poll refreshes every interval until ctx ends.
func (c *Controller) Poll(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.mu.TryLock() {
				c.refresh()
				c.mu.Unlock()
			}
		}
	}
}
