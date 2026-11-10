package agentdetect

import (
	"context"
	"path"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Input is everything Classify looks at: the pane's current command and the
// pid of its process (the tmux pane_pid, the root of the process tree), and
// the sanitised screen. Any part may be empty.
type Input struct {
	// Command is the pane's current foreground command as the host reports it
	// (for example "bash", "node" or "claude"). A leading "-" (login shell)
	// and a path are ignored.
	Command string
	// PID is the pane's process id. With a ProcessSource it is the root of the
	// tree searched for a wrapped agent. Zero means unknown.
	PID int
	// Screen is the sanitised visible screen (AD-14).
	Screen ops.Screen
}

// Choice is one option of a dialog: the label as shown and the complete input
// that the answer op sends to pick it, so that the answer op stays
// agent-agnostic ("answer sends the chosen option's keys").
type Choice struct {
	Label string `json:"label"`
	// Keys is the input that selects and confirms this option, in order, each
	// element a tmux key name ("Enter", "Right") or a literal character
	// ("1", "y"):
	//
	//   - a numbered menu that selects on the digit gives ["1"], with no
	//     Enter (an extra Enter would answer the next prompt);
	//   - a lettered or yes/no prompt gives ["y", "Enter"];
	//   - a menu driven by the arrow keys gives the moves from the first
	//     option and the confirmation, for example ["Right", "Enter"].
	Keys []string `json:"keys"`
}

// Result is the classification of a pane. Kind and State are always members
// of the closed sets of AD-12 (ops.PaneKind*, ops.PaneState*).
type Result struct {
	Kind  string
	State string
	// Choices are the parsed options when State is dialog, in screen order.
	// It is empty for every other state, and also for a dialog whose options
	// have no addressable key.
	Choices []Choice
}

// Agent returns the CLI name when Kind is "agent:<cli>", otherwise "".
func (r Result) Agent() string {
	return strings.TrimPrefix(r.Kind, ops.PaneKindAgentPrefix)
}

// IsAgent reports whether the pane runs a known agent CLI.
func (r Result) IsAgent() bool {
	return strings.HasPrefix(r.Kind, ops.PaneKindAgentPrefix) && len(r.Kind) > len(ops.PaneKindAgentPrefix)
}

// Valid reports whether Kind and State belong to the closed sets.
func (r Result) Valid() bool {
	kindOK := r.Kind == ops.PaneKindShell || r.Kind == ops.PaneKindOther || r.Kind == ops.PaneKindUnknown || r.IsAgent()
	switch r.State {
	case ops.PaneStateStable, ops.PaneStateBusy, ops.PaneStateDialog, ops.PaneStateUnknown:
		return kindOK
	}
	return false
}

// Proc is one process of the process table.
//
// PGRP, TPGID and State carry the job-control facts that AD-12 needs: only a
// process in the foreground process group of the pane's terminal, and not
// stopped, can be the agent that receives typed input.
type Proc struct {
	PID  int
	PPID int
	// PGRP is the process group of the process. TPGID is the foreground
	// process group of the terminal the process is attached to (zero or
	// negative when it has none). Zero means unknown.
	PGRP int
	// TTY identifies the controlling terminal (the device number on Linux,
	// the ps name elsewhere). Empty means none or unknown.
	TTY   string
	TPGID int
	// State is the run state: the first letter is the Linux or BSD state
	// code (R, S, D, T, t, Z, ...). Empty means unknown, treated as running.
	State string
	Args  []string // argv, or the command line split on whitespace
}

// Foreground reports whether the process is in the foreground process group
// of its own terminal. Unknown data (zero) is not foreground: the
// classification must not authorise input to a process that may be in the
// background. A process on another terminal (a nested pty) can be foreground
// there; use inPaneForeground for the pane's terminal.
func (p Proc) Foreground() bool { return p.PGRP > 0 && p.PGRP == p.TPGID }

// inPaneForeground reports whether p receives the input typed into the pane:
// p and the pane's root process (tmux pane_pid, the session leader of the
// pane's terminal) are attached to the same known terminal, and p is in that
// terminal's foreground process group. The root passes this test only while
// it is itself in the foreground group, so a wrapper whose child (an
// interactive shell) took the terminal is not the input's recipient.
func inPaneForeground(root, p Proc) bool {
	return p.Foreground() && root.TTY != "" && p.TTY == root.TTY && p.TPGID == root.TPGID
}

// Running reports whether the process is neither stopped (Ctrl-Z) nor a
// zombie nor dead.
func (p Proc) Running() bool {
	if p.State == "" {
		return true
	}
	switch p.State[0] {
	case 'T', 't', 'Z', 'X', 'x':
		return false
	}
	return true
}

// ProcessSource is the port to the process table. Implementations return a
// snapshot of all processes. An error means the tree is unavailable; the
// classification then falls back to the current command alone.
type ProcessSource interface {
	Processes(ctx context.Context) ([]Proc, error)
}

// Detector classifies panes against a table of known agents.
type Detector struct {
	agents []Agent
	procs  ProcessSource
}

// Option configures a Detector.
type Option func(*Detector)

// WithAgents replaces the table of known agents.
func WithAgents(agents []Agent) Option { return func(d *Detector) { d.agents = agents } }

// WithProcessSource sets the process table. A nil source disables the
// process-tree search.
func WithProcessSource(p ProcessSource) Option { return func(d *Detector) { d.procs = p } }

// New returns a Detector with DefaultAgents and the process source of the
// platform (see DefaultProcessSource).
func New(opts ...Option) *Detector {
	d := &Detector{agents: DefaultAgents(), procs: DefaultProcessSource()}
	for _, o := range opts {
		o(d)
	}
	return d
}

var defaultDetector = New()

// Classify classifies a pane with the default Detector.
func Classify(ctx context.Context, in Input) Result { return defaultDetector.Classify(ctx, in) }

// Search limits of the process tree walk: a pane tree is shallow, and the
// bounds keep a pathological table from costing time.
const (
	maxTreeDepth = 6
	maxTreeProcs = 256
)

// Classify returns the kind and state of the pane. It never fails: missing or
// contradictory evidence yields kind "unknown" or "other" with state
// "unknown".
//
// Kind: a current command that is a known agent is that agent. A shell or an
// interpreter (node, python, ...) may wrap one, so the process tree below PID
// is searched, nearest process first; an agent found there makes the pane
// agent:<cli> (an agent under "bash -c claude" is still the agent). Only
// processes that receive the pane's input count, the root included: they
// are attached to the pane's own terminal (an agent on a nested pty, for
// example under a background "script -c claude", does not), are in its
// foreground process group, and are not stopped or zombies. A stopped
// (Ctrl-Z) or background agent leaves the pane's input with the shell, and a
// message must never be typed into a shell (AD-12). The argv of a shell
// running a compound "-c" script ("claude; bash -i") is never evidence: the
// shell outlives the agent, so a live agent process is required. Without an agent, a shell is "shell" and any other
// command is "other"; an empty command with nothing found is "unknown".
//
// State: for an agent pane, from the screen (dialog, then busy, then the idle
// prompt, else unknown). For a shell, "busy" while a foreground job runs and
// "stable" otherwise (background jobs and helpers do not make it busy),
// "unknown" when the process table is unavailable. For every other kind it
// is "unknown".
func (d *Detector) Classify(ctx context.Context, in Input) Result {
	cmd := progName(in.Command)
	if a := d.matchProgram(cmd); a != nil {
		return d.agentResult(a, in.Screen)
	}

	var (
		tree      []Proc
		haveTable bool
	)
	wrapper := cmd == "" || shells[cmd] || isInterpreter(cmd)
	if wrapper && in.PID > 0 && d.procs != nil {
		if all, err := d.procs.Processes(ctx); err == nil {
			haveTable = true
			tree = descendants(all, in.PID)
		}
	}
	for _, p := range tree {
		if !p.Running() || !inPaneForeground(tree[0], p) {
			continue
		}
		if a := d.matchArgs(p.Args); a != nil {
			return d.agentResult(a, in.Screen)
		}
	}

	switch {
	case shells[cmd]:
		r := Result{Kind: ops.PaneKindShell, State: ops.PaneStateUnknown}
		if haveTable {
			r.State = ops.PaneStateStable
			if hasForegroundJob(tree, in.PID) {
				r.State = ops.PaneStateBusy
			}
		}
		return r
	case cmd == "":
		return Result{Kind: ops.PaneKindUnknown, State: ops.PaneStateUnknown}
	default:
		return Result{Kind: ops.PaneKindOther, State: ops.PaneStateUnknown}
	}
}

func (d *Detector) agentResult(a *Agent, scr ops.Screen) Result {
	state, choices := screenState(a, scr)
	return Result{Kind: ops.PaneKindAgentPrefix + a.Name, State: state, Choices: choices}
}

// matchProgram returns the agent whose Commands contain the normalised
// program name.
func (d *Detector) matchProgram(prog string) *Agent {
	if prog == "" {
		return nil
	}
	for i := range d.agents {
		for _, c := range d.agents[i].Commands {
			if prog == strings.ToLower(c) {
				return &d.agents[i]
			}
		}
	}
	return nil
}

// matchToken matches one argument by program name and by ArgHints.
func (d *Detector) matchToken(t string) *Agent {
	if a := d.matchProgram(progName(scriptName(t))); a != nil {
		return a
	}
	for i := range d.agents {
		for _, h := range d.agents[i].ArgHints {
			if strings.Contains(t, h) {
				return &d.agents[i]
			}
		}
	}
	return nil
}

// progName normalises a program name: base name, no login-shell dash, no
// ".exe", lower case.
func progName(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), `\`, "/")
	s = path.Base(s)
	if s == "." || s == "/" {
		return ""
	}
	s = strings.TrimPrefix(s, "-")
	s = strings.TrimSuffix(strings.ToLower(s), ".exe")
	return s
}

// scriptName strips the script extensions that carry no meaning for a match.
func scriptName(t string) string {
	for _, ext := range []string{".js", ".mjs", ".cjs", ".py"} {
		if strings.HasSuffix(t, ext) {
			return strings.TrimSuffix(t, ext)
		}
	}
	return t
}

// descendants returns the process with the given pid followed by its
// descendants in breadth-first order, bounded by maxTreeDepth and
// maxTreeProcs. The root comes first even when it is absent from the table.
func descendants(all []Proc, root int) []Proc {
	children := make(map[int][]Proc, len(all))
	byPID := make(map[int]Proc, len(all))
	for _, p := range all {
		children[p.PPID] = append(children[p.PPID], p)
		byPID[p.PID] = p
	}
	var out []Proc
	if p, ok := byPID[root]; ok {
		out = append(out, p)
	} else {
		out = append(out, Proc{PID: root})
	}
	visited := map[int]bool{root: true}
	level := []int{root}
	for depth := 0; depth < maxTreeDepth && len(level) > 0 && len(out) < maxTreeProcs; depth++ {
		var next []int
		for _, pid := range level {
			for _, c := range children[pid] {
				if visited[c.PID] || len(out) >= maxTreeProcs {
					continue
				}
				visited[c.PID] = true
				out = append(out, c)
				next = append(next, c.PID)
			}
		}
		level = next
	}
	return out
}

// hasForegroundJob reports whether the tree holds a running process other
// than the root that is in the pane terminal's foreground process group: a
// job the shell is waiting for. Stopped jobs, background jobs and helpers are not.
func hasForegroundJob(tree []Proc, root int) bool {
	for _, p := range tree {
		if p.PID != root && p.Running() && inPaneForeground(tree[0], p) {
			return true
		}
	}
	return false
}
