// Package tray is the logic of the desktop tray (cmd/minion-tray): what the
// menu shows and which `minion` command each menu item runs. It imports no GUI
// library; the menu and the dialogs are behind the Dialogs and View
// interfaces, so the handlers are tested with a fake `minion` child process.
//
// Every action is a `minion` command; the tray reads nothing but the output of
// `minion worker status --json` and `minion worker config share list --json`.
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
	Connection   struct {
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

// SessionsCouldEnd is true when stopping may end open terminal sessions: any
// counted session, an unknown count, or the ConPTY host (its sessions live
// inside the Worker).
func (s Status) SessionsCouldEnd() bool {
	if !s.Running() {
		return false
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

// ShareList is `minion worker config share list --json`.
type ShareList struct {
	Mode          string   `json:"mode"`
	Revision      string   `json:"revision"`
	Locked        bool     `json:"locked"`
	Selected      []string `json:"selected"`
	SessionsKnown bool     `json:"sessions_known"`
	Sessions      []struct {
		Name   string `json:"name"`
		Shared bool   `json:"shared"`
	} `json:"sessions"`
}

// Names are the sessions to offer and the ones checked now. Where the host
// cannot list its sessions (Windows ConPTY) the saved selection is all there is.
func (l ShareList) Names() (all, checked []string) {
	if !l.SessionsKnown {
		return append([]string{}, l.Selected...), append([]string{}, l.Selected...)
	}
	for _, s := range l.Sessions {
		all = append(all, s.Name)
		if s.Shared {
			checked = append(checked, s.Name)
		}
	}
	return all, checked
}

// Dialogs are the native dialogs. A cancelled dialog returns ok false.
type Dialogs interface {
	// Token asks for the join token (input hidden).
	Token() (token string, ok bool)
	// Confirm asks a yes/no question.
	Confirm(text string) bool
	// Pick shows a checklist; checked names are preselected.
	Pick(text string, all, checked []string) (chosen []string, ok bool)
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
	c.refresh()
	if c.last.SessionsCouldEnd() && !c.D.Confirm("Stopping the Worker can end the terminal sessions that are open. Stop it?") {
		return
	}
	c.do("", "worker", "down")
	c.refresh()
}

// TogglePause is `minion worker pause` or `resume`, by the current status.
func (c *Controller) TogglePause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh()
	if c.last.Paused {
		c.do("", "worker", "resume")
	} else {
		c.do("", "worker", "pause")
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
	if c.do(tok+"\n", "worker", "up", "--join", "-") {
		c.D.Info("The Worker is set up. Check the Hub key words in the menu against the ones your Hub shows.")
	}
	c.refresh()
}

// ShareSessions is the session picker: `config share list --json`, then
// `config share select --revision N -- names` (or `none`).
func (c *Controller) ShareSessions() {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, err := c.shareList()
	if err != nil {
		c.D.Error(err.Error())
		return
	}
	all, checked := l.Names()
	text := "Choose the sessions the Hub may show."
	if l.Locked {
		text += " (The Hub cannot change this choice; you can.)"
	}
	if len(all) == 0 {
		c.D.Info("This host has no sessions to share yet.")
		return
	}
	chosen, ok := c.D.Pick(text, all, checked)
	if !ok {
		return
	}
	args := []string{"worker", "config", "share"}
	if len(chosen) == 0 {
		args = append(args, "none")
	} else {
		args = append(args, "select")
	}
	if l.Revision != "" {
		args = append(args, "--revision", l.Revision)
	}
	if len(chosen) > 0 {
		args = append(args, "--")
		args = append(args, chosen...)
	}
	if _, err := c.run("", args...); err != nil {
		if again, lerr := c.shareList(); lerr == nil && again.Revision != l.Revision {
			c.D.Error("The sharing choice changed while you were choosing. Nothing was changed; open the picker again to see the current choice.")
		} else {
			c.D.Error(err.Error())
		}
		return
	}
	c.refresh()
}

func (c *Controller) shareList() (ShareList, error) {
	var l ShareList
	out, err := c.run("", "worker", "config", "share", "list", "--json")
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		return l, fmt.Errorf("cannot read the session list of minion: %w", err)
	}
	return l, nil
}

// ToggleNewSessions flips `config set allow_new_sessions`. The checkbox shows
// the status; unknown counts as on, so a click blocks.
func (c *Controller) ToggleNewSessions() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh()
	next := "off"
	if c.last.NewSessions == "off" {
		next = "on"
	}
	c.do("", "worker", "config", "set", "allow_new_sessions", next)
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
