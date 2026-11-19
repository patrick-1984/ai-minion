package trayactions

import (
	"fmt"
	"strings"
)

// Program is the executable an action runs.
type Program string

// The two programs of the tray app.
const (
	// Minion is the headless minion binary (the CLI); the tray finds it as
	// its sibling or at a configured path, never through PATH.
	Minion Program = "minion"
	// MinionTray is the tray app itself (autostart, about, quit).
	MinionTray Program = "minion-tray"
)

// Stdin is what the tray writes to the standard input of the command.
type Stdin string

const (
	// StdinNone: the command reads nothing.
	StdinNone Stdin = "none"
	// StdinToken: the join token the user entered, followed by a newline. It
	// is a secret: it never appears in argv, dialog text or a log.
	StdinToken Stdin = "token"
)

// Confirm is the confirmation the tray asks for.
type Confirm string

const (
	// ConfirmNone: the action runs at once.
	ConfirmNone Confirm = "none"
	// ConfirmSessionLoss: the command may refuse because sessions would end
	// (exit 23 with sessions_lost N, or 29 when they cannot be counted). The
	// tray then names the loss ("N sessions will end", or "an unknown number
	// of sessions will end") and, after a yes, runs the argv again with
	// [AcceptLossArgv] or [AcceptUnknownLossArgv] appended.
	ConfirmSessionLoss Confirm = "session_loss"
	// ConfirmLeave: always asks first (Hub address, Hub words, Worker name,
	// state directory, the sessions that end, and that the Worker stays
	// listed on the Hub until it is removed there); the session-loss retry of
	// ConfirmSessionLoss applies too.
	ConfirmLeave Confirm = "leave"
)

// SessionLoss reports whether the session-loss retry applies to c.
func (c Confirm) SessionLoss() bool { return c == ConfirmSessionLoss || c == ConfirmLeave }

// Elevate is the elevated run a Windows tray may do after the unelevated
// command refused with exit 21 (needs_admin): one of a closed set of two
// argv (contract E1/E2), run only on the registered and verified service
// binary. There is no elevated restart, join, leave, update or "worker up".
type Elevate string

const (
	// ElevateNone: never elevated.
	ElevateNone Elevate = "none"
	// ElevateE1 is "minion worker start --json".
	ElevateE1 Elevate = "E1"
	// ElevateE2 is "minion worker stop --scm-only --accept-unknown-loss
	// --json", only after the unelevated stop returned 21, or 29 with an
	// unreachable pipe, and the user confirmed that an unknown number of
	// sessions will end.
	ElevateE2 Elevate = "E2"
)

// ElevatedArgv returns the exact argv of an elevated run, nil for
// [ElevateNone] or an unknown value. The set is closed: these are the only
// argv a tray ever runs elevated.
func ElevatedArgv(e Elevate) []string {
	switch e {
	case ElevateE1:
		return []string{"worker", "start", "--json"}
	case ElevateE2:
		return []string{"worker", "stop", "--scm-only", "--accept-unknown-loss", "--json"}
	}
	return nil
}

// Effect is the kind of effect of an action.
type Effect string

const (
	// EffectState changes the Worker's state; Observe names the status JSON
	// path that shows it.
	EffectState Effect = "state"
	// EffectRead changes nothing; the result is shown in the menu.
	EffectRead Effect = "read"
	// EffectBrowser opens an address in the browser.
	EffectBrowser Effect = "browser"
	// EffectClipboard puts words on the clipboard.
	EffectClipboard Effect = "clipboard"
	// EffectTerminal runs the command in a terminal window, interactively.
	EffectTerminal Effect = "terminal"
	// EffectTray changes the tray app itself (autostart entry, process).
	EffectTray Effect = "tray"
)

// Effects lists every effect kind.
var Effects = []Effect{EffectState, EffectRead, EffectBrowser, EffectClipboard, EffectTerminal, EffectTray}

// Action is one control of the tray menu and its command line.
type Action struct {
	// ID is stable: tests, "minion tray-actions --json" and the docs use it.
	ID string `json:"id"`
	// Menu is the menu path of the control, as the tray shows it.
	Menu string `json:"menu"`
	// Program is the executable: "minion" or "minion-tray".
	Program Program `json:"program"`
	// Argv are the arguments after the program name. An argument that is a
	// whole "{name}" is a placeholder (see [Placeholders]); [Resolve]
	// substitutes it.
	Argv []string `json:"argv"`
	// Optional are argument groups the tray appends when it has their value
	// (for example "--hub-fingerprint {hub_fp}").
	Optional [][]string `json:"optional,omitempty"`
	// Stdin is what the tray writes to the command's standard input.
	Stdin Stdin `json:"stdin"`
	// Confirm is the confirmation the tray asks for.
	Confirm Confirm `json:"confirm"`
	// Elevate is the elevated retry on Windows after exit 21.
	Elevate Elevate `json:"elevate"`
	// Effect is the kind of effect.
	Effect Effect `json:"effect"`
	// Observe says how the effect is seen: a path of the status JSON
	// ("minion worker status --json") with its expected value, or the
	// observation for effects outside the Worker.
	Observe string `json:"observe"`
}

// AcceptLossArgv is appended after the user accepted that {n} sessions end
// (exit 23 named n).
var AcceptLossArgv = []string{"--accept-loss", "{n}"}

// AcceptUnknownLossArgv is appended after the user accepted that an unknown
// number of sessions end (exit 29).
var AcceptUnknownLossArgv = []string{"--accept-unknown-loss"}

// Placeholders are the names an argv may use, with what the tray puts there.
// "{sessions}" stands for zero or more arguments; every other placeholder for
// exactly one.
var Placeholders = map[string]string{
	"state_dir": "profile.state_dir of the tray's profile (C7)",
	"n":         "sessions_lost of the refusal (exit 23)",
	"session":   "the session name of the menu item",
	"sessions":  "the names of the currently shared sessions (share.sessions), zero or more arguments",
	"revision":  "the revision of minion worker share list --json",
	"share":     "all, none or later, chosen in a dialog",
	"url":       "the Hub address entered in a dialog",
	"hub_fp":    "the Hub fingerprint (six words) entered in a dialog",
	"attempt":   "pairing.attempt of the status",
	"fp":        "worker_fingerprint of the status (may be empty)",
	"epoch":     "enrollment_epoch of the status",
}

// listPlaceholder is the one placeholder that stands for zero or more
// arguments.
const listPlaceholder = "sessions"

// PlaceholderName returns the name of a whole-argument placeholder "{name}",
// or "" when arg is a literal.
func PlaceholderName(arg string) string {
	if len(arg) > 2 && strings.HasPrefix(arg, "{") && strings.HasSuffix(arg, "}") {
		return arg[1 : len(arg)-1]
	}
	return ""
}

// Resolve substitutes the placeholders of argv with values. Every
// placeholder must be known and have a value; "{sessions}" takes any number
// of values, every other placeholder exactly one. Literal arguments are kept
// as they are.
func Resolve(argv []string, values map[string][]string) ([]string, error) {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		name := PlaceholderName(a)
		if name == "" {
			if strings.ContainsAny(a, "{}") {
				return nil, fmt.Errorf("argument %q has a placeholder that is not a whole argument", a)
			}
			out = append(out, a)
			continue
		}
		if _, ok := Placeholders[name]; !ok {
			return nil, fmt.Errorf("unknown placeholder {%s}", name)
		}
		v, ok := values[name]
		if !ok {
			return nil, fmt.Errorf("no value for {%s}", name)
		}
		if name != listPlaceholder && len(v) != 1 {
			return nil, fmt.Errorf("{%s} takes exactly one value, got %d", name, len(v))
		}
		out = append(out, v...)
	}
	return out, nil
}

// ByID returns the action with id.
func ByID(id string) (Action, bool) {
	for _, a := range All {
		if a.ID == id {
			return a, true
		}
	}
	return Action{}, false
}

// Shorthands of the table.
var (
	statusArgv = []string{"worker", "status", "--state-dir", "{state_dir}", "--json"}
	shareOpt   = []string{"--share-existing", "{share}"}
)

func state(id, menu string, argv []string, observe string) Action {
	return Action{ID: id, Menu: menu, Program: Minion, Argv: argv, Stdin: StdinNone, Confirm: ConfirmNone, Elevate: ElevateNone, Effect: EffectState, Observe: observe}
}

func read(id, menu, observe string) Action {
	return Action{ID: id, Menu: menu, Program: Minion, Argv: append([]string(nil), statusArgv...), Stdin: StdinNone, Confirm: ConfirmNone, Elevate: ElevateNone, Effect: EffectRead, Observe: observe}
}

func with(a Action, f func(*Action)) Action {
	f(&a)
	return a
}

func tray(id, menu string, argv []string, observe string) Action {
	return Action{ID: id, Menu: menu, Program: MinionTray, Argv: argv, Stdin: StdinNone, Confirm: ConfirmNone, Elevate: ElevateNone, Effect: EffectTray, Observe: observe}
}

// All is the action table, in menu order (06-architecture-v2 §5 and §6).
var All = []Action{
	read("status", "status line, icon and badges", "the whole status JSON (source of every menu state)"),
	read("hubkey.show", "Hub key line (or MISMATCH)", "hub_key.state, hub_key.line (verbatim)"),
	read("workerkey.show", "This Worker: name and key words", "name, worker_fingerprint_words"),
	read("hub.paircode", "Pairing code (only while pending)", "pairing.state == pending, pairing.code"),

	state("pause.30m", "Pause ▸ for 30 minutes", []string{"worker", "pause", "--for", "30m", "--state-dir", "{state_dir}", "--json"}, "paused == true, paused_until set"),
	state("pause.2h", "Pause ▸ for 2 hours", []string{"worker", "pause", "--for", "2h", "--state-dir", "{state_dir}", "--json"}, "paused == true, paused_until set"),
	state("pause.indef", "Pause ▸ until I resume", []string{"worker", "pause", "--state-dir", "{state_dir}", "--json"}, "paused == true, paused_until empty"),
	state("resume", "Resume", []string{"worker", "resume", "--state-dir", "{state_dir}", "--json"}, "paused == false"),

	with(state("start", "Start the Worker", []string{"worker", "start", "--state-dir", "{state_dir}", "--json"}, "live.running == true, live.state_dir == profile.state_dir"),
		func(a *Action) { a.Elevate = ElevateE1 }),
	with(state("stop", "Stop the Worker…", []string{"worker", "stop", "--state-dir", "{state_dir}", "--json"}, "live.running == false"),
		func(a *Action) { a.Confirm, a.Elevate = ConfirmSessionLoss, ElevateE2 }),
	with(state("restart", "Restart the Worker…", []string{"worker", "restart", "--state-dir", "{state_dir}", "--json"}, "live.instance changed, live.state_dir unchanged"),
		func(a *Action) { a.Confirm = ConfirmSessionLoss }),

	with(state("share.list", "Sessions shared with the Hub ▸ (contents)", []string{"worker", "share", "list", "--state-dir", "{state_dir}", "--json"}, "rule, revision, sessions[].shared of the result"),
		func(a *Action) { a.Effect = EffectRead }),
	state("share.all", "Sessions shared with the Hub ▸ All", []string{"worker", "share", "set", "all", "--if-revision", "{revision}", "--state-dir", "{state_dir}", "--json"}, "share.mode == all"),
	state("share.none", "Sessions shared with the Hub ▸ None", []string{"worker", "share", "set", "none", "--if-revision", "{revision}", "--state-dir", "{state_dir}", "--json"}, "share.mode == none"),
	state("share.select", "Sessions shared with the Hub ▸ Selected", []string{"worker", "share", "set", "select", "{sessions}", "--if-revision", "{revision}", "--state-dir", "{state_dir}", "--json"}, "share.mode == select, share.sessions unchanged"),
	state("share.add", "Sessions shared with the Hub ▸ ☑ {session}", []string{"worker", "share", "add", "{session}", "--if-revision", "{revision}", "--state-dir", "{state_dir}", "--json"}, "share.sessions contains {session}"),
	state("share.remove", "Sessions shared with the Hub ▸ ☐ {session}", []string{"worker", "share", "remove", "{session}", "--if-revision", "{revision}", "--state-dir", "{state_dir}", "--json"}, "share.sessions lacks {session}"),

	state("block.on", "☑ Block new sessions created through Minion", []string{"worker", "config", "set", "allow_new_sessions", "off", "--state-dir", "{state_dir}", "--json"}, "new_sessions.blocked == true, new_sessions.in_force == true"),
	state("block.off", "☐ Block new sessions created through Minion", []string{"worker", "config", "set", "allow_new_sessions", "on", "--state-dir", "{state_dir}", "--json"}, "new_sessions.blocked == false, new_sessions.in_force == true"),

	with(state("hub.open", "Hub ▸ Open the Hub", []string{"worker", "open-hub", "--state-dir", "{state_dir}", "--json"}, "the browser opens hub_web_url (url of the result)"),
		func(a *Action) { a.Effect = EffectBrowser }),
	with(state("hub.join", "Hub ▸ Join with a join token…", []string{"worker", "join", "--token-file", "-", "--state-dir", "{state_dir}", "--json"}, "paired == true, live.running == true (enrollment, config, activation of the result)"),
		func(a *Action) { a.Optional, a.Stdin, a.Confirm = [][]string{shareOpt}, StdinToken, ConfirmSessionLoss }),
	with(state("hub.join.resume", "Hub ▸ Finish joining", []string{"worker", "join", "--resume", "--state-dir", "{state_dir}", "--json"}, "paired == true, live.running == true"),
		func(a *Action) { a.Confirm = ConfirmSessionLoss }),
	with(state("hub.pair", "Hub ▸ Pair with a code…", []string{"worker", "join", "{url}", "--background", "--state-dir", "{state_dir}", "--json"}, "pairing.state == pending, then paired == true"),
		func(a *Action) {
			a.Optional, a.Confirm = [][]string{{"--hub-fingerprint", "{hub_fp}"}, shareOpt}, ConfirmSessionLoss
		}),
	state("hub.cancelpair", "Hub ▸ Cancel pairing", []string{"worker", "pair", "cancel", "--attempt", "{attempt}", "--state-dir", "{state_dir}", "--json"}, "pairing.state != pending, paired == false"),
	with(state("hub.leave", "Hub ▸ Leave this Hub…", []string{"worker", "leave", "--yes", "--state-dir", "{state_dir}", "--expect-worker-fingerprint", "{fp}", "--expect-enrollment-epoch", "{epoch}", "--json"}, "paired == false, hub_url empty, live.running == false"),
		func(a *Action) { a.Confirm = ConfirmLeave }),

	with(state("copy.hubkey", "Copy ▸ Hub key words", []string{"worker", "copy", "hub-key", "--state-dir", "{state_dir}", "--json"}, "the clipboard holds hub_key.words"),
		func(a *Action) { a.Effect = EffectClipboard }),
	with(state("copy.workerkey", "Copy ▸ Worker key words", []string{"worker", "copy", "worker-key", "--state-dir", "{state_dir}", "--json"}, "the clipboard holds worker_fingerprint_words"),
		func(a *Action) { a.Effect = EffectClipboard }),

	with(state("setup", "Set up the Worker…", []string{"worker", "up", "--state-dir", "{state_dir}"}, "lifecycle.installed == true"),
		func(a *Action) { a.Effect = EffectTerminal }),

	tray("autostart.on", "☑ Start the tray at login", []string{"autostart", "on"}, "the autostart entry exists (minion-tray autostart status)"),
	tray("autostart.off", "☐ Start the tray at login", []string{"autostart", "off"}, "no autostart entry (minion-tray autostart status)"),
	with(tray("about.version", "About ▸ tray, minion and Worker versions", []string{"--version"}, "the tray version; cli_version and live.version of the status"),
		func(a *Action) { a.Effect = EffectRead }),
	with(tray("about.commands", "About ▸ CLI commands…", []string{"commands"}, "the output equals this parity table"),
		func(a *Action) { a.Effect = EffectRead }),
	with(state("about.docs", "About ▸ Documentation", []string{"docs", "tray", "--json"}, "the browser opens the tray guide (url of the result)"),
		func(a *Action) { a.Effect = EffectBrowser }),
	tray("quit", "Quit the tray", []string{"quit"}, "the tray process is gone"),
}
