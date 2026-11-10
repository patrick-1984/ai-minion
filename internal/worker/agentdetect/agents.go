package agentdetect

import (
	"regexp"
	"strings"
)

// Agent describes one known agent CLI: how its process is recognised and what
// its screen looks like in each state. The table is extensible: pass your own
// slice with WithAgents, or append to DefaultAgents().
type Agent struct {
	// Name is the CLI name used in the kind ("agent:" + Name).
	Name string
	// Commands are the executable base names that denote the agent, matched
	// case-insensitively against the pane's current command and against the
	// program or script names in its process tree.
	Commands []string
	// ArgHints are substrings of an argument (a package or script path such
	// as "@google/gemini-cli") that denote the agent when it runs under an
	// interpreter like node.
	ArgHints []string
	// Idle and Busy hold the screen markers. They are matched against the
	// bottom lines of the sanitised screen, in addition to the generic
	// markers.
	Idle []*regexp.Regexp
	Busy []*regexp.Regexp
	// Dialog marks a dialog whose options form a single row below the marker line
	// and are driven by the arrow keys (opencode: "Allow once   Allow always
	// Reject"). Choices are parsed from that row, each selected by the arrow
	// moves from the currently selected option (read from the style spans of
	// the screen) plus Enter; when no row is found, or the selected option
	// cannot be told, the state is still dialog and Choices stays empty.
	Dialog []*regexp.Regexp
	// DialogFooter marks the footer line of a choice dialog of this agent
	// (for example "Esc to cancel"). A numbered option list followed by or
	// accompanied by such a footer is a dialog even without a question line.
	DialogFooter []*regexp.Regexp
}

func rx(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }

// genericBusy and genericIdle apply to every agent in addition to its own
// markers. Busy markers are the "press esc to stop" hints and spinner glyphs
// that agent CLIs print while a turn runs.
var (
	genericBusy = []*regexp.Regexp{
		rx(`(?i)\b(esc|ctrl\+c)\b[^\n]{0,12}\bto (interrupt|cancel|stop)\b`),
		rx(`^\s*[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]\s`),
	}
	genericIdle = []*regexp.Regexp{
		rx(`\? for shortcuts`),
	}
	genericDialogFooter = []*regexp.Regexp{
		rx(`(?i)esc to (cancel|exit)`),
		rx(`(?i)enter to (confirm|select)`),
	}
)

// DefaultAgents returns a fresh copy of the table of known agent CLIs.
//
// The screen markers follow the interfaces of the supported releases: the
// golden screens in testdata/ are the reference, and a CLI that changes its
// interface is a one-line change in this table.
func DefaultAgents() []Agent {
	return []Agent{
		{
			Name:     "claude",
			Commands: []string{"claude", "claude-code"},
			ArgHints: []string{"@anthropic-ai/claude-code"},
			Idle: []*regexp.Regexp{
				rx(`(?i)\b(auto-accept edits|plan mode|bypass permissions)\b[^\n]*\bon\b`),
				rx(`^\s*[│|]\s*[>❯](\s|$)`),
			},
			Busy: []*regexp.Regexp{rx(`(?i)esc to interrupt`)},
		},
		{
			Name:     "codex",
			Commands: []string{"codex"},
			ArgHints: []string{"@openai/codex"},
			Idle: []*regexp.Regexp{
				rx(`⏎ send`),
				rx(`^\s*›(\s|$)`),
			},
			Busy: []*regexp.Regexp{rx(`(?i)working\s*\(`)},
		},
		{
			Name:     "gemini",
			Commands: []string{"gemini"},
			ArgHints: []string{"@google/gemini-cli"},
			Idle:     []*regexp.Regexp{rx(`Type your message or @path`)},
		},
		{
			Name:     "qwen",
			Commands: []string{"qwen", "qwen-code"},
			ArgHints: []string{"@qwen-code/qwen-code"},
			Idle:     []*regexp.Regexp{rx(`Type your message or @path`)},
		},
		{
			Name:     "aider",
			Commands: []string{"aider"},
			ArgHints: []string{"aider-chat"},
			Idle:     []*regexp.Regexp{rx(`^[A-Za-z-]*>\s*$`)},
			Busy: []*regexp.Regexp{
				rx(`(?i)^\s*(waiting for|applying edits|committing)\b`),
			},
		},
		{
			Name:     "opencode",
			Commands: []string{"opencode"},
			ArgHints: []string{"opencode-ai"},
			Idle: []*regexp.Regexp{
				rx(`(?i)\bask anything\b`),
				rx(`(?i)\bctrl\+p commands\b`),
			},
			Busy:   []*regexp.Regexp{rx(`(?i)esc (again )?to interrupt|esc interrupt`)},
			Dialog: []*regexp.Regexp{rx(`(?i)permission required`)},
		},
	}
}

// interpreters are programs that run an agent script or package: when one of
// them is the current command, the agent may be found in its arguments or its
// children. Versioned names such as python3.12 are handled by isInterpreter.
var interpreters = map[string]bool{
	"node": true, "nodejs": true, "bun": true, "deno": true, "npx": true, "pnpm": true,
	"npm": true, "yarn": true, "python": true, "python3": true, "uv": true, "uvx": true,
	"pipx": true, "env": true, "sudo": true, "script": true,
}

// shells are the interactive shells reported as kind "shell".
var shells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ash": true,
	"ksh": true, "mksh": true, "tcsh": true, "csh": true, "nu": true, "nushell": true,
	"elvish": true, "xonsh": true, "pwsh": true, "powershell": true, "cmd": true,
}

// isInterpreter reports whether prog (a normalised program name) is a script
// or package runner, including versioned names such as python3.12.
func isInterpreter(prog string) bool {
	if interpreters[prog] {
		return true
	}
	return strings.HasPrefix(prog, "python")
}
