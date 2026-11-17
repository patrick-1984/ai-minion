package web

// The Debug panel of the Worker detail page (v0.10.0): the owner of a Worker or
// an administrator switches the Worker's debug logging on for a while, or off,
// and reads the recent lines of its log (worker_debug, worker_logs). The
// Worker serves both only when it was started with allow_debug_remote; the
// panel says so, with the command to allow it, instead of offering buttons that
// would be refused. Every call is audited by the Hub, the panel's own reads
// included.

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const (
	pathDebugPattern = "/w/{worker}/debug"
	// panelLogLines is how many log lines the panel shows unless asked for
	// another number.
	panelLogLines = 200
)

// DebugPath is the Debug panel of a Worker. GET renders the closed panel and
// calls nothing; every call of the Worker is a POST through the CSRF check of
// the session (action=logs reads the state and the log, enabled=1 or 0
// switches debug logging), because each one is audited in the person's name.
func DebugPath(worker string) string { return "/w/" + url.PathEscape(worker) + "/debug" }

// debugLineChoices are the numbers of lines the panel offers.
var debugLineChoices = []int{100, 200, 500, 1000, 2000}

// DebugView is what the "debug_panel" fragment sees.
type DebugView struct {
	WorkerID string
	Worker   string
	URL      string
	// Note says why the panel offers nothing (not allowed, offline, too old);
	// Command is the command that allows remote debug on the Worker.
	Note    string
	Command string
	// Available is true when the Worker serves remote debug to this person.
	Available bool
	// Closed is set until the person opens the panel: the page load does not
	// read the Worker's log (each read is audited).
	Closed    bool
	State     ops.WorkerDebugState
	Remaining string
	// Log is the tail of the Worker's log; Lines the number asked for.
	// LogError says why the log could not be read; the state and the on/off
	// controls stay.
	Log      string
	LogError string
	// StateUnknown is set when neither the switch nor the log read told the
	// state: the panel then offers both on and off.
	StateUnknown bool
	Shown        int
	Lines        int
	Choices      []int
	Results      []ActionResult
}

var debugTemplates = sync.OnceValue(func() *template.Template {
	return template.Must(template.ParseFS(templateFiles, "templates/debug.html"))
})

// debugAllowCommand is what the panel tells people to run on the Worker.
const debugAllowCommand = "minion worker up --foreground --allow-debug-remote   (or for the service: minion worker config set allow_debug_remote true, then restart the Worker)"

// debugView builds the panel without calling the Worker when it cannot serve.
func debugView(t target) (DebugView, bool) {
	v := DebugView{WorkerID: t.worker.ID, Worker: t.worker.Name, URL: DebugPath(t.worker.ID), Lines: panelLogLines, Choices: debugLineChoices}
	switch {
	case !t.operate:
		v.Note = "Only the owner of this worker, or an administrator, can switch its debug logging and read its log."
	case !t.worker.Online:
		v.Note = "This worker is offline. Its debug logging can be switched when it reconnects."
	case !slices.Contains(t.worker.Ops, ops.OpWorkerDebug) || !slices.Contains(t.worker.Ops, ops.OpWorkerLogs):
		v.Note = "This worker is too old for remote debug. Update it to v0.10.0 or later; until then start it with --log-level debug and read its log on the machine."
	case !t.worker.DebugRemote:
		v.Note = "This worker does not allow remote debug (allow_debug_remote is off, the default). To allow it, run this on the worker machine:"
		v.Command = debugAllowCommand
	default:
		v.Available = true
	}
	return v, v.Available
}

// fillLogs reads the log tail (and with it the debug state) into v.
func fillLogs(r *http.Request, t target, v *DebugView) error {
	var res ops.WorkerLogsResult
	if err := t.c.CallOp(r, ops.OpWorkerLogs, ops.WorkerLogsArgs{WorkerID: t.worker.ID, Lines: v.Lines}, &res); err != nil {
		return err
	}
	v.State = res.WorkerDebugState
	v.Remaining = remainingText(res.RemainingSeconds)
	v.Log = strings.Join(res.Lines, "\n")
	v.Shown = len(res.Lines)
	return nil
}

// remainingText is "about N minutes" for a remaining time in seconds.
func remainingText(sec int) string {
	switch {
	case sec <= 0:
		return ""
	case sec < 60:
		return "less than a minute"
	case sec < 120:
		return "about 1 minute"
	}
	return fmt.Sprintf("about %d minutes", sec/60)
}

// debugFailure turns a refusal into the panel's text; the Worker's own
// refusal names the command that allows remote debug.
func debugFailure(err error) string {
	var oe *OpError
	if errors.As(err, &oe) {
		switch oe.Code {
		case "worker_offline":
			return "This worker is offline. Its debug logging can be switched when it reconnects."
		case "unsupported_op":
			return "This worker is too old for remote debug. Update it to v0.10.0 or later."
		case "forbidden":
			if oe.Message != "" {
				return oe.Message
			}
		}
	}
	return failureText(err)
}

// parseLines reads the lines choice of a request; anything else is the default.
func parseLines(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || !slices.Contains(debugLineChoices, n) {
		return panelLogLines
	}
	return n
}

// handleDebugGet renders the panel without calling the Worker: reading its
// log is audited, so it takes an intended, CSRF-checked POST.
func handleDebugGet(w http.ResponseWriter, r *http.Request, t target) {
	v, ok := debugView(t)
	v.Lines = parseLines(r.URL.Query().Get("lines"))
	v.Closed = ok
	writeFragmentTemplate(w, debugTemplates(), "debug_panel", v)
}

// handleDebugLogs opens or refreshes the panel: the state and the log tail.
// A Worker that refuses or is gone shows the reason instead of the controls.
func handleDebugLogs(w http.ResponseWriter, r *http.Request, t target, v DebugView) {
	if err := fillLogs(r, t, &v); err != nil {
		v.Available = false
		v.Results = []ActionResult{{Label: "Debug", Kind: "danger", Summary: debugFailure(err)}}
	}
	writeFragmentTemplate(w, debugTemplates(), "debug_panel", v)
}

// fillLogsAfter reads the log after a switch whose answer gave the state
// already: a failed read keeps that state and the controls, and only the log
// area says what went wrong.
func fillLogsAfter(r *http.Request, t target, v *DebugView, st *ops.WorkerDebugState) {
	if err := fillLogs(r, t, v); err != nil {
		v.LogError = "The log could not be read: " + debugFailure(err)
		if st != nil {
			v.State, v.Remaining = *st, remainingText(st.RemainingSeconds)
		} else {
			v.StateUnknown = true
		}
	}
}

func handleDebugSet(w http.ResponseWriter, r *http.Request, t target) {
	v, ok := debugView(t)
	v.Lines = parseLines(r.PostForm.Get("lines"))
	if !ok {
		writeFragmentTemplate(w, debugTemplates(), "debug_panel", v)
		return
	}
	if r.PostForm.Get("action") == "logs" {
		handleDebugLogs(w, r, t, v)
		return
	}
	args := ops.WorkerDebugArgs{WorkerID: t.worker.ID, Enabled: r.PostForm.Get("enabled") == "1"}
	if m := strings.TrimSpace(r.PostForm.Get("minutes")); m != "" && args.Enabled {
		n, err := strconv.Atoi(m)
		if err != nil || n < 1 || n > ops.MaxDebugMinutes {
			v.Results = []ActionResult{{Label: "Debug", Kind: "danger", Summary: fmt.Sprintf("Minutes must be 1 to %d.", ops.MaxDebugMinutes)}}
			fillLogsAfter(r, t, &v, nil)
			writeFragmentTemplate(w, debugTemplates(), "debug_panel", v)
			return
		}
		args.Minutes = n
	}
	var st ops.WorkerDebugState
	ar := ActionResult{Label: "Debug"}
	known := &st
	if err := t.c.CallOp(r, ops.OpWorkerDebug, args, &st); err != nil {
		ar.Kind, ar.Summary = "danger", debugFailure(err)
		known = nil
	} else if args.Enabled {
		ar.Kind, ar.Summary = "info", "Debug logging is on; it switches itself off in "+remainingText(st.RemainingSeconds)+"."
	} else {
		ar.Kind, ar.Summary = "info", "Remote debug logging is off. The worker logs at its configured level ("+st.Level+")."
	}
	if known != nil {
		// The switch answered with the state: render it from that answer, so
		// the off switch stays even when the log read below fails.
		v.State, v.Remaining = st, remainingText(st.RemainingSeconds)
	}
	fillLogsAfter(r, t, &v, known)
	v.Results = []ActionResult{ar}
	writeFragmentTemplate(w, debugTemplates(), "debug_panel", v)
}

func init() {
	RegisterRoute(Route{Pattern: "GET " + pathDebugPattern, Handler: sessionRoute(handleDebugGet)})
	RegisterRoute(Route{Pattern: "POST " + pathDebugPattern, Handler: sessionRoute(handleDebugSet)})
}
