package web

// The interactive session view (Story 5.5, R46, R49, R87, UX-DR6; AD-21, AD-14,
// AD-15). /s/<worker>/<session> shows the screen of one session and lets the
// user type, press keys, set the task and answer a dialog.
//
// The page never touches a Worker or a store. Every read and write is one
// operation of the catalog (workers_list, session_snapshot, send_text,
// send_keys, set_task, answer, session_create) called over the REST pipeline as
// the signed-in user, so authorisation (AD-10, AD-11), the Worker-side guards
// (AD-12) and the audit trail are those of the API. The page hides the input
// controls of a user who cannot operate the session, but that is only a
// convenience: the handlers below forward every request and the API refuses it
// (defence in depth).
//
// Rendering (AD-14). The screen is the ops.Screen of the snapshot: text lines
// plus style spans. [NewScreenView] cuts every line into runs and
// _screen.html prints each run through html/template, so the text is always
// escaped and a style is only ever a class name built here from validated
// colour codes. The Content-Security-Policy forbids inline styles, so the
// colours are classes; static/terminal-input.js defines them in a constructed
// style sheet.
//
// Live refresh. The browser listens to the Hub's event feed
// (/api/v1/events, Story 5.3) and fetches the panel fragment again when a
// screen_changed or state_changed event names this session.
//
// Write flow (UX-DR6). Each form posts to a route below, which calls the
// operation with a fresh request id and answers with the result fragment:
// outcome, reason and warnings. A FORCE-class refusal (forceable=true) comes
// with a "Send anyway" form that posts the same fields again with force=1; the
// handler makes a new request id for it, because the Worker would answer the old
// id from its cache.
//
// Atomic submit and reviewed dialogs (EPICS A-22, spine AM-9). Typed text and its
// Enter are one send_text call with submit=true: one request id, one pane, one
// Worker lock, so the Enter cannot reach another pane or follow another writer's
// text. A dialog answer carries the tokens of the snapshot that rendered the
// panel (pane id, pid, command, session generation, dialog digest) as hidden
// fields, and the handler sends them as "expect" of the answer op. The Worker
// refuses an answer whose tokens no longer match (pane_changed or
// dialog_changed, never forceable). The page never refreshes the tokens by
// itself: after such a refusal it asks the browser to drop the typed answer and
// show the current screen, and the user reads the new dialog and answers again.

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Paths of the session view. The patterns use net/http wildcards; the helpers
// below build the escaped paths of one session.
const (
	pathSessionPattern    = "/s/{worker}/{session}"
	pathNewSessionPattern = "/w/{worker}/sessions"

	// eventsPath is the Hub's SSE feed (api.EventsPath, Story 5.3). The web
	// package does not import the API package; a test keeps the two equal.
	eventsPath = "/api/v1/events"
)

// Limits of the session routes.
const (
	// maxSessionFormBytes bounds the body of a session form post.
	maxSessionFormBytes = 256 << 10
	// maxSnapshotErrorRunes bounds an error message shown in the panel.
	maxSnapshotErrorRunes = 300
	// maxCursorPad bounds the blanks added to show a cursor beyond a line.
	maxCursorPad = 512
)

// SessionPath returns the path of the view of one session. worker is the
// Worker's name (or its id); both parts are escaped.
func SessionPath(worker, session string) string {
	return "/s/" + url.PathEscape(worker) + "/" + url.PathEscape(session)
}

// NewSessionPath returns the path that creates a session on a Worker (POST).
// The Workers page (Story 5.4) uses it for its "new session" button.
func NewSessionPath(worker string) string {
	return "/w/" + url.PathEscape(worker) + "/sessions"
}

// -- the screen model ----------------------------------------------------------

// ScreenView is a screen ready for _screen.html: lines of runs.
type ScreenView struct {
	Lines []ScreenLine
}

// ScreenLine is one row of the screen.
type ScreenLine struct {
	Runs []ScreenRun
}

// ScreenRun is a piece of a line with one style. Class is empty for the default
// style; otherwise it holds only class names built by this package. Text is
// plain text: the template escapes it.
type ScreenRun struct {
	Text  string
	Class string
}

// NewScreenView cuts a screen into styled runs. It does not trust the spans:
// a span with a bad range, an overlapping span and a colour outside the
// screen model are skipped, so a Worker that breaks the contract can only make
// a plain line. The cursor becomes a run with the class "t-cur".
func NewScreenView(s ops.Screen) ScreenView {
	v := ScreenView{Lines: make([]ScreenLine, len(s.Lines))}
	spans := make([][]ops.StyleSpan, len(s.Lines))
	for _, sp := range s.Spans {
		if sp.Line >= 0 && sp.Line < len(s.Lines) {
			spans[sp.Line] = append(spans[sp.Line], sp)
		}
	}
	for i, line := range s.Lines {
		cursor := -1
		if s.Cursor != nil && s.Cursor.Row == i && s.Cursor.Col >= 0 {
			cursor = s.Cursor.Col
		}
		v.Lines[i] = ScreenLine{Runs: lineRuns([]rune(line), spans[i], cursor)}
	}
	return v
}

func lineRuns(runes []rune, spans []ops.StyleSpan, cursor int) []ScreenRun {
	var runs []ScreenRun
	emit := func(from, to int, class string) {
		if from >= to {
			return
		}
		if cursor >= from && cursor < to {
			runs = appendRun(runs, string(runes[from:cursor]), class)
			runs = appendRun(runs, string(runes[cursor:cursor+1]), joinClass(class, "t-cur"))
			runs = appendRun(runs, string(runes[cursor+1:to]), class)
			return
		}
		runs = appendRun(runs, string(runes[from:to]), class)
	}
	pos := 0
	for _, sp := range spans {
		if sp.Start < pos || sp.End <= sp.Start || sp.End > len(runes) {
			continue // out of order, overlapping or out of range: plain text
		}
		emit(pos, sp.Start, "")
		emit(sp.Start, sp.End, spanClass(sp))
		pos = sp.End
	}
	emit(pos, len(runes), "")
	if cursor >= len(runes) {
		pad := min(cursor-len(runes), maxCursorPad)
		runs = appendRun(runs, strings.Repeat(" ", pad), "")
		runs = appendRun(runs, " ", "t-cur")
	}
	return runs
}

func appendRun(runs []ScreenRun, text, class string) []ScreenRun {
	if text == "" {
		return runs
	}
	// Neighbours of one style are one run, so a skipped span leaves no seam.
	if n := len(runs); n > 0 && runs[n-1].Class == class {
		runs[n-1].Text += text
		return runs
	}
	return append(runs, ScreenRun{Text: text, Class: class})
}

func joinClass(a, b string) string {
	if a == "" {
		return b
	}
	return a + " " + b
}

// spanClass turns the style of a span into class names. An inverse span swaps
// its colours; a missing colour then stands for the default one of the other
// side.
func spanClass(sp ops.StyleSpan) string {
	var c []string
	for _, f := range []struct {
		on  bool
		cls string
	}{{sp.Bold, "t-b"}, {sp.Dim, "t-dim"}, {sp.Italic, "t-i"}, {sp.Underline, "t-u"}, {sp.Strike, "t-s"}} {
		if f.on {
			c = append(c, f.cls)
		}
	}
	fg, bg := colorClass("fg", sp.FG), colorClass("bg", sp.BG)
	if sp.Inverse {
		fg, bg = colorClass("fg", sp.BG), colorClass("bg", sp.FG)
		if fg == "" {
			fg = "t-fg-bgdef"
		}
		if bg == "" {
			bg = "t-bg-fgdef"
		}
	}
	for _, x := range []string{fg, bg} {
		if x != "" {
			c = append(c, x)
		}
	}
	return strings.Join(c, " ")
}

// colorClass maps a colour of the screen model ("p0" to "p255", "#rrggbb") to
// a class name; an invalid or empty colour gives "".
func colorClass(kind, c string) string {
	if c == "" || !ops.ValidColor(c) {
		return ""
	}
	if c[0] == '#' {
		return "t-" + kind + "x-" + c[1:]
	}
	return "t-" + kind + "-" + c[1:]
}

// -- the page model ------------------------------------------------------------

// KeyButton is one key button of the session view.
type KeyButton struct {
	// Label is the text of the button; Name is the key as a result names it.
	Label string
	Name  string
	// Key is the tmux key name sent by send_keys.
	Key string
	// Note explains a key that can do harm; empty for the others.
	Note string
}

// sessionKeys are the key buttons, in display order. The keys route accepts
// only these. Enter is its own action ("Send Enter"), never a side effect of
// typing text.
var sessionKeys = []KeyButton{
	{Label: "Send Enter", Name: "Enter", Key: "Enter"},
	{Label: "Esc", Name: "Esc", Key: "Escape"},
	{Label: "Interrupt (Ctrl-C)", Name: "Ctrl-C", Key: "C-c", Note: "Interrupt can stop a command that is running."},
	{Label: "Tab", Name: "Tab", Key: "Tab"},
	{Label: "Shift-Tab", Name: "Shift-Tab", Key: "BTab"},
	{Label: "Space", Name: "Space", Key: "Space"},
	{Label: "Backspace", Name: "Backspace", Key: "BSpace"},
	{Label: "Delete", Name: "Delete", Key: "DC"},
	{Label: "Up", Name: "Up", Key: "Up"}, {Label: "Down", Name: "Down", Key: "Down"},
	{Label: "Left", Name: "Left", Key: "Left"}, {Label: "Right", Name: "Right", Key: "Right"},
}

// answerChoices are the quick answers of a dialog: the Worker takes a digit as
// the position of an option and a single character as that key; the answer box
// takes anything else (an option label).
var answerChoices = []string{"1", "2", "3", "y", "n"}

// PanelView is the part of the page that live refresh replaces: the screen, the
// pane state and, while a dialog is open, the answer controls.
type PanelView struct {
	Screen ScreenView
	// Kind, State, Width and Height describe the pane; zero values are not shown.
	Kind   string
	State  string
	Width  int
	Height int
	// Dialog says that the pane shows a dialog; Answer is the form to answer it
	// and is nil for a user who cannot operate the session.
	Dialog bool
	Answer *AnswerView
	// AnswerNote explains why an operator has no answer form while a dialog is
	// open (the Worker does not report the identity of the dialog).
	AnswerNote string
	// Captured is when the Hub read this screen ("15:04:05 UTC"); CapturedAt is
	// the same instant for the time element.
	Captured   string
	CapturedAt string
	// StateLabel is State as people read it.
	StateLabel string
}

// AnswerView is the dialog answer form. Reviewed holds the tokens of the
// snapshot the panel was rendered from; both forms post them back as hidden
// fields.
type AnswerView struct {
	URL      string
	Choices  []string
	Reviewed []FormField
}

// Names of the hidden fields that carry the reviewed dialog of an answer form.
const (
	fieldExpectPaneID    = "expect_pane_id"
	fieldExpectPID       = "expect_pid"
	fieldExpectCommand   = "expect_command"
	fieldExpectCreated   = "expect_session_created"
	fieldExpectDigest    = "expect_dialog_digest"
	answerUnboundMessage = "This Worker does not report the identity of the dialog, so the page cannot answer it safely. Update the Worker, or answer in the terminal."
)

// reviewedFields turns the pane of a snapshot into the hidden fields of an
// answer form. It returns false when the Worker did not report all of them.
func reviewedFields(p ops.PaneInfo) ([]FormField, bool) {
	rd := reviewedOfPane(p)
	if rd.Validate() != nil {
		return nil, false
	}
	return []FormField{
		{fieldExpectPaneID, rd.PaneID},
		{fieldExpectPID, strconv.Itoa(rd.PID)},
		{fieldExpectCommand, rd.Command},
		{fieldExpectCreated, strconv.FormatInt(rd.SessionCreated, 10)},
		{fieldExpectDigest, rd.DialogDigest},
	}, true
}

func reviewedOfPane(p ops.PaneInfo) ops.ReviewedDialog {
	return ops.ReviewedDialog{PaneID: p.ID, PID: p.PID, Command: p.Command, SessionCreated: p.SessionCreated, DialogDigest: p.DialogDigest}
}

// reviewedFromForm reads the reviewed dialog back from a posted answer form.
// It returns false when a token is missing or malformed: the page then makes no
// call, because an answer without the dialog the user saw must never be sent.
func reviewedFromForm(r *http.Request) (ops.ReviewedDialog, bool) {
	pid, err1 := strconv.Atoi(r.PostFormValue(fieldExpectPID))
	created, err2 := strconv.ParseInt(r.PostFormValue(fieldExpectCreated), 10, 64)
	if err1 != nil || err2 != nil {
		return ops.ReviewedDialog{}, false
	}
	rd := ops.ReviewedDialog{
		PaneID: r.PostFormValue(fieldExpectPaneID), PID: pid, Command: r.PostFormValue(fieldExpectCommand),
		SessionCreated: created, DialogDigest: r.PostFormValue(fieldExpectDigest),
	}
	return rd, rd.Validate() == nil
}

// SessionPage is what session.html sees as .Data.
type SessionPage struct {
	Worker    string
	WorkerID  string
	Owner     string
	Session   string
	Online    bool
	Task      string
	Notice    string
	Operate   bool
	Panel     PanelView
	Keys      []KeyButton
	ScreenURL string
	EventsURL string
	TextURL   string
	KeysURL   string
	TaskURL   string
	NewURL    string
	// Context is the public identity context of the request (see page_inspector_context.go).
	Context string
	// Panes are the views of the right pane, in order. The built-in one is
	// "Details"; other stories add theirs with [RegisterSessionPane].
	Panes []PaneView
	// Conversation is the rendered conversation component of the compact (embedded) view, which has no
	// pane: the same HTML as the "conversation" pane, so the component exists once per document.
	Conversation template.HTML
	// HasConversation: the conversation view is registered and rendered, so its script is loaded.
	HasConversation bool
}

// Crumbs implements [Crumbed]: a session lives below the session list.
func (p SessionPage) Crumbs() []Crumb {
	return []Crumb{{Label: "Control center", Path: "/"}, {Label: "Sessions", Path: pathSessions}, {Label: p.Session}}
}

// FormField is a hidden field of the force form.
type FormField struct {
	Name  string
	Value string
}

// ForceForm repeats a refused call with force=1. Target names the session the
// repeat would write to; the confirmation dialog shows it.
type ForceForm struct {
	Action string
	Fields []FormField
	Target string
}

// ActionResult is the outcome of one call, as the result fragment shows it.
type ActionResult struct {
	// Label says what was tried ("Text", "Key Enter", "Task", "Answer").
	Label string
	// Kind is "info", "warning" or "danger"; it picks the flash colour.
	Kind    string
	Summary string
	// Reason is the reason code of a refusal or failure; empty when none.
	Reason   string
	Warnings []string
	Force    *ForceForm
	// Target is the session the call was for ("minion-1 on pi-1") and At the
	// time the Hub answered ("15:04:05 UTC"), so a result never gets read as the
	// result of another session or an earlier attempt.
	Target string
	At     string
	// Submitted is the text of a send whose outcome the page cannot confirm; the
	// operator can read it again before deciding to send it once more.
	Submitted string
}

// ResultView is what the "result" template sees.
type ResultView struct {
	Results []ActionResult
}

// outcomeSummary is the sentence for an outcome of AD-15.
func outcomeSummary(res ops.WriteResult) (kind, summary string) {
	switch res.Outcome {
	case ops.OutcomeDelivered:
		kind, summary = "info", "Delivered."
		if res.Forced {
			summary = "Delivered (sent anyway)."
		}
	case ops.OutcomeFailedNotSent:
		kind, summary = "danger", "Not sent"+reasonSuffix(res.Reason)+"."
		if res.Forceable {
			summary = "Not sent" + reasonSuffix(res.Reason) + ". Nothing reached the pane."
		}
	case ops.OutcomePartial:
		kind, summary = "warning", "Partly sent"+reasonSuffix(res.Reason)+". Check the screen before you send anything else."
	default:
		kind, summary = "warning", "Delivery is unknown"+reasonSuffix(res.Reason)+". Inspect the session before deciding whether to send again."
	}
	return kind, summary
}

// resultOf builds the ActionResult of a write result. force is the form that a
// "send anyway" button would post; it is used only for a forceable refusal.
func resultOf(label string, res ops.WriteResult, force *ForceForm) ActionResult {
	kind, summary := outcomeSummary(res)
	ar := ActionResult{Label: label, Kind: kind, Summary: summary, Reason: res.Reason}
	for _, w := range res.Warnings {
		ar.Warnings = append(ar.Warnings, w.Message)
	}
	if len(ar.Warnings) > 0 && ar.Kind == "info" {
		ar.Kind = "warning"
	}
	if res.Forceable && !res.Forced {
		ar.Force = force
	}
	return ar
}

// errorResult is the ActionResult of a call the API refused or failed.
func errorResult(label string, err error) ActionResult {
	var se *StatusError
	if errors.As(err, &se) {
		return ActionResult{Label: label, Kind: "danger", Summary: se.Message}
	}
	return ActionResult{Label: label, Kind: "danger", Summary: failureText(err)}
}

// -- the caller and the session -------------------------------------------------

// sessionCaller returns the operation caller the Hub's web module connected
// (the same one as the administration pages).
func sessionCaller() (OpCaller, error) {
	b := adminOps.Load()
	if b == nil {
		return nil, &StatusError{Code: http.StatusServiceUnavailable, Message: "The session view is not connected to the Hub yet."}
	}
	return b.c, nil
}

// requestUser returns the user whose space the request works in and whether
// that user is an admin. While an admin acts as another user, the API treats the
// session as that user, so the page does too.
func requestUser(r *http.Request) (id string, admin, ok bool) {
	sess, found := session.FromContext(r.Context())
	if !found {
		return "", false, false
	}
	if sess.ActAs != nil {
		return sess.ActAs.TargetID, false, true
	}
	return sess.UserID, sess.Role == authz.RoleAdmin, true
}

// findWorker finds the Worker that a path names. A name is looked up in the
// caller's own Workers first; the id always works (an admin who sees Workers of
// several owners with the same name uses it). Several matches that cannot be
// told apart count as not found.
func findWorker(list []ops.WorkerInfo, ref, userID string) (ops.WorkerInfo, bool) {
	for _, w := range list {
		if w.ID == ref {
			return w, true
		}
	}
	var own, other []ops.WorkerInfo
	for _, w := range list {
		if w.Name != ref {
			continue
		}
		if w.OwnerID == userID {
			own = append(own, w)
		} else {
			other = append(other, w)
		}
	}
	switch {
	case len(own) == 1:
		return own[0], true
	case len(own) == 0 && len(other) == 1:
		return other[0], true
	}
	return ops.WorkerInfo{}, false
}

// target is a resolved session of a request.
type target struct {
	c       OpCaller
	worker  ops.WorkerInfo
	ref     string // the path segment that named the Worker
	session string
	operate bool
}

func (t target) opTarget() ops.Target {
	return ops.Target{WorkerID: t.worker.ID, SessionName: t.session}
}

func (t target) path() string { return SessionPath(t.ref, t.session) }

// resolveWorker looks the Worker of the request up through workers_list.
func resolveWorker(r *http.Request, c OpCaller) (ops.WorkerInfo, string, bool, error) {
	uid, admin, ok := requestUser(r)
	if !ok {
		return ops.WorkerInfo{}, "", false, ErrNotFound()
	}
	var res ops.WorkersListResult
	if err := c.CallOp(r, ops.OpWorkersList, ops.WorkersListArgs{All: admin}, &res); err != nil {
		return ops.WorkerInfo{}, "", false, pageError(err)
	}
	ref := r.PathValue("worker")
	w, found := findWorker(res.Workers, ref, uid)
	if !found {
		return ops.WorkerInfo{}, "", false, ErrNotFound()
	}
	// The base permission of AD-11: an admin may operate everything, the owner
	// operates the own Workers. The API decides again on every call.
	return w, ref, admin || w.OwnerID == uid, nil
}

func resolveTarget(r *http.Request) (target, error) {
	c, err := sessionCaller()
	if err != nil {
		return target{}, err
	}
	w, ref, operate, err := resolveWorker(r, c)
	if err != nil {
		return target{}, err
	}
	return target{c: c, worker: w, ref: ref, session: r.PathValue("session"), operate: operate}, nil
}

// -- the page ------------------------------------------------------------------

// panelFor reads the snapshot of the session. A Worker that is offline has no
// screen; a refused or failed snapshot is an error for the caller.
func panelFor(r *http.Request, t target) (PanelView, error) {
	var snap ops.SessionSnapshotResult
	if err := t.c.CallOp(r, ops.OpSessionSnapshot, ops.SessionSnapshotArgs{Target: t.opTarget()}, &snap); err != nil {
		return PanelView{}, err
	}
	now := time.Now().UTC()
	p := PanelView{Screen: NewScreenView(snap.Screen), Kind: snap.Pane.Kind, State: snap.Pane.State,
		Width: snap.Screen.Width, Height: snap.Screen.Height,
		Captured: now.Format("15:04:05") + " UTC", CapturedAt: now.Format(time.RFC3339), StateLabel: stateLabel(snap.Pane.State)}
	if snap.Pane.State == ops.PaneStateDialog {
		p.Dialog = true
		if t.operate {
			if reviewed, ok := reviewedFields(snap.Pane); ok {
				p.Answer = &AnswerView{URL: t.path() + "/answer", Choices: answerChoices, Reviewed: reviewed}
			} else {
				p.AnswerNote = answerUnboundMessage
			}
		}
	}
	return p, nil
}

// sessionTask reads the task of the session from sessions_list; it is a
// convenience, so a failure shows an empty field.
func sessionTask(r *http.Request, t target) string {
	var res ops.SessionsListResult
	args := struct {
		WorkerID string `json:"worker_id"`
	}{t.worker.ID}
	if err := t.c.CallOp(r, ops.OpSessionsList, args, &res); err != nil {
		return ""
	}
	for _, s := range res.Sessions {
		if s.Name == t.session {
			return s.Task
		}
	}
	return ""
}

func sessionData(r *http.Request) (any, error) {
	t, err := resolveTarget(r)
	if err != nil {
		return nil, err
	}
	page := SessionPage{
		Worker: t.worker.Name, WorkerID: t.worker.ID, Owner: t.worker.Owner, Session: t.session, Online: t.worker.Online,
		Keys: sessionKeys, ScreenURL: t.path() + "/screen",
		EventsURL: eventsPath + "?all=true&worker=" + url.QueryEscape(t.worker.ID),
		TextURL:   t.path() + "/text", KeysURL: t.path() + "/keys", TaskURL: t.path() + "/task",
		NewURL: NewSessionPath(t.ref), Context: principalContext(r),
	}
	if !t.worker.Online {
		page.Notice = "This Worker is offline. The screen and the controls come back when it reconnects."
		page.Panes = renderSessionPanes(r, t, page)
		page.Conversation = conversationOf(page.Panes)
		page.HasConversation = page.Conversation != ""
		return page, nil
	}
	panel, err := panelFor(r, t)
	if err != nil {
		var oe *OpError
		if errors.As(err, &oe) && oe.Code == "worker_offline" {
			page.Online = false
			page.Notice = "This Worker is offline. The screen and the controls come back when it reconnects."
			page.Panes = renderSessionPanes(r, t, page)
			page.Conversation = conversationOf(page.Panes)
			page.HasConversation = page.Conversation != ""
			return page, nil
		}
		return nil, pageError(err)
	}
	page.Panel = panel
	page.Operate = t.operate
	if t.operate {
		page.Task = sessionTask(r, t)
	}
	page.Panes = renderSessionPanes(r, t, page)
	page.Conversation = conversationOf(page.Panes)
	page.HasConversation = page.Conversation != ""
	return page, nil
}

// conversationOf returns the HTML of the conversation pane.
func conversationOf(panes []PaneView) template.HTML {
	for _, p := range panes {
		if p.ID == "conversation" {
			return p.HTML
		}
	}
	return ""
}

// -- templates -----------------------------------------------------------------

// sessionTemplates parses session.html with the screen partial, so that a route
// can render the "panel" and "result" fragments the file defines.
var sessionTemplates = sync.OnceValue(func() *template.Template {
	return template.Must(template.ParseFS(templateFiles, "templates/session.html", "templates/_screen.html"))
})

func renderFragment(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := sessionTemplates().ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "the answer could not be rendered", http.StatusInternalServerError)
		return
	}
	writeFragment(w, buf.Bytes())
}

// -- the routes ----------------------------------------------------------------

// sessionRoute wraps a handler of the session view: a connected caller, the
// resolved Worker (a Worker the user cannot see is a plain 404), and a bounded
// form body.
func sessionRoute(fn func(w http.ResponseWriter, r *http.Request, t target)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			r.Body = http.MaxBytesReader(w, r.Body, maxSessionFormBytes)
			if err := r.ParseForm(); err != nil {
				renderFragment(w, "result", ResultView{Results: []ActionResult{{Label: "Request", Kind: "danger", Summary: "The form could not be read."}}})
				return
			}
		}
		t, err := resolveTarget(r)
		if err != nil {
			var se *StatusError
			if errors.As(err, &se) && r.Method == http.MethodGet {
				http.Error(w, se.Message, se.Code)
				return
			}
			renderFragment(w, "result", ResultView{Results: []ActionResult{errorResult("Request", err)}})
			return
		}
		fn(w, r, t)
	})
}

// handlePanel answers the screen fragment that live refresh swaps in.
func handlePanel(w http.ResponseWriter, r *http.Request, t target) {
	if !t.worker.Online {
		http.Error(w, "the Worker is offline", http.StatusServiceUnavailable)
		return
	}
	panel, err := panelFor(r, t)
	if err != nil {
		status := http.StatusBadGateway
		var oe *OpError
		if errors.As(err, &oe) && (oe.Status == http.StatusNotFound || oe.Status == http.StatusForbidden) {
			status = http.StatusNotFound
		}
		http.Error(w, truncateRunes(failureText(err), maxSnapshotErrorRunes), status)
		return
	}
	renderFragment(w, "panel", panel)
}

// answerResult ends a write route: the result fragment, and the events that
// tell the browser to fetch the screen again and, after typed text (a text or a
// typed dialog answer) was delivered, that its input is done (minion-input-sent).
func answerResult(w http.ResponseWriter, t target, results []ActionResult, sentText bool) {
	answerResultEvents(w, t, results, sentText, false)
}

// answerResultEvents is answerResult with the event that ends a refused answer:
// stale says that the dialog the user reviewed is gone (minion-dialog-stale), so
// the browser drops the typed answer and shows the current screen.
func answerResultEvents(w http.ResponseWriter, t target, results []ActionResult, sentText, stale bool) {
	answerResultTrigger(w, t, results, sentText, stale, false)
}

// resultEvents are the events a write route sends to the browser in HX-Trigger.
type resultEvents struct {
	// sent: the text was delivered (minion-input-sent), so the box may be cleared.
	sent bool
	// stale: the dialog the user reviewed is gone (minion-dialog-stale).
	stale bool
	// taskSaved: the task label was stored (minion-task-saved).
	taskSaved bool
	// unknown is the outcome the Worker could not confirm ("unknown" or
	// "partial") with its reason code; empty when there is none. The browser keeps
	// the payload it submitted outside the replaceable result until the operator
	// dismisses it (minion-input-unknown). Only codes travel in the header.
	unknown, unknownReason string
	// pipe: a pipe message was sent (any outcome): the conversation reads the stored turns again (minion-pipe-refresh).
	pipe bool
}

// withOutcome adds the unknown/partial signal of a write result.
func (e resultEvents) withOutcome(ok bool, res ops.WriteResult) resultEvents {
	if !ok {
		return e
	}
	switch res.Outcome {
	case ops.OutcomeUnknown:
		e.unknown, e.unknownReason = "unknown", res.Reason
	case ops.OutcomePartial:
		e.unknown, e.unknownReason = "partial", res.Reason
	}
	return e
}

// answerResultTrigger also tells the browser that a task was saved
// (minion-task-saved), so the field stops showing unsaved changes.
func answerResultTrigger(w http.ResponseWriter, t target, results []ActionResult, sentText, stale, taskSaved bool) {
	answerResultSignals(w, t, results, resultEvents{sent: sentText, stale: stale, taskSaved: taskSaved})
}

// answerResultSignals renders the results and sends the events of ev.
func answerResultSignals(w http.ResponseWriter, t target, results []ActionResult, ev resultEvents) {
	trigger := `{"minion-screen-refresh":true`
	if ev.sent {
		trigger += `,"minion-input-sent":true`
	}
	if ev.pipe {
		trigger += `,"minion-pipe-refresh":true`
	}
	if ev.stale {
		trigger += `,"minion-dialog-stale":true`
	}
	if ev.taskSaved {
		trigger += `,"minion-task-saved":true`
	}
	if ev.unknown != "" {
		detail, _ := json.Marshal(map[string]string{"outcome": ev.unknown, "reason": ev.unknownReason})
		trigger += `,"minion-input-unknown":` + string(detail)
	}
	w.Header().Set("HX-Trigger", trigger+"}")
	now := time.Now().UTC().Format("15:04:05") + " UTC"
	for i := range results {
		if results[i].Target == "" {
			results[i].Target = t.label()
		}
		if results[i].At == "" {
			results[i].At = now
		}
	}
	renderFragment(w, "result", ResultView{Results: results})
}

// label names the session of a request for a result: "minion-1 on pi-1".
func (t target) label() string { return t.session + " on " + t.worker.Name }

// forceForm is the form that repeats a refused call with force=1.
func (t target) forceForm(action string, fields []FormField) *ForceForm {
	return &ForceForm{Action: action, Fields: fields, Target: t.label()}
}

// maxSubmittedRunes bounds the submitted text a result repeats.
const maxSubmittedRunes = 2000

// blankText reports a text with nothing but white space (the page's trim() also counts U+FEFF).
func blankText(s string) bool {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == '\uFEFF' }) == ""
}

// forced reports whether the form carries force=1 ("send anyway").
func forced(r *http.Request) bool { return r.PostFormValue("force") == "1" }

// callWrite runs one write operation with a fresh request id.
func callWrite(r *http.Request, t target, op string, args any, label string, force *ForceForm) (ActionResult, ops.WriteResult, bool) {
	var res ops.WriteResult
	if err := t.c.CallOp(r, op, args, &res); err != nil {
		return errorResult(label, err), res, false
	}
	return resultOf(label, res, force), res, true
}

func handleSendText(w http.ResponseWriter, r *http.Request, t target) {
	// A browser posts line breaks as CRLF; the Worker takes LF and refuses CR.
	text := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(r.PostFormValue("text"))
	if text == "" {
		answerResult(w, t, []ActionResult{{Label: "Text", Kind: "danger", Summary: "Type some text first."}}, false)
		return
	}
	enter := r.PostFormValue("enter") == "1"
	if enter && blankText(text) {
		// The primary Send is the same action as Enter in the box: a whitespace-only draft is never sent and never becomes a
		// remote Enter. An explicit paste without Enter (enter=0) keeps its whitespace, and the native send_text API is unchanged.
		answerResult(w, t, []ActionResult{{Label: "Text", Kind: "danger", Summary: "Type some text first. Nothing was sent. To press Enter in the session on its own, use Send Enter in Keys."}}, false)
		return
	}
	fields := []FormField{{"text", text}}
	if enter {
		fields = append(fields, FormField{"enter", "1"})
	}
	// The text and its Enter are one call: one request id, one pane, one Worker
	// lock. A refused call is repeated as the same single call with force=1, so
	// the Enter can never go to a pane the text did not reach.
	ff := t.forceForm(t.path()+"/text", fields)
	args := ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7(), Force: forced(r)}, Target: t.opTarget(), Text: text, Submit: enter}
	ar, res, ok := callWrite(r, t, ops.OpSendText, args, "Text", ff)
	if ok && (res.Outcome == ops.OutcomeUnknown || res.Outcome == ops.OutcomePartial) {
		// The text stays in the box too; this copy survives an edit of the box.
		ar.Submitted = truncateRunes(text, maxSubmittedRunes)
	}
	answerResultSignals(w, t, []ActionResult{ar}, resultEvents{sent: ok && res.Outcome == ops.OutcomeDelivered}.withOutcome(ok, res))
}

func handleSendKey(w http.ResponseWriter, r *http.Request, t target) {
	key := r.PostFormValue("key")
	label := ""
	for _, k := range sessionKeys {
		if k.Key == key {
			label = k.Name
		}
	}
	if label == "" {
		answerResult(w, t, []ActionResult{{Label: "Key", Kind: "danger", Summary: "That key is not one of the buttons."}}, false)
		return
	}
	args := ops.SendKeysArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7(), Force: forced(r)}, Target: t.opTarget(), Keys: []string{key}}
	ff := t.forceForm(t.path()+"/keys", []FormField{{"key", key}})
	ar, res, ok := callWrite(r, t, ops.OpSendKeys, args, "Key "+label, ff)
	answerResultSignals(w, t, []ActionResult{ar}, resultEvents{}.withOutcome(ok, res))
}

func handleSetTask(w http.ResponseWriter, r *http.Request, t target) {
	task := strings.TrimSpace(r.PostFormValue("task"))
	args := ops.SetTaskArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7(), Force: forced(r)}, Target: t.opTarget(), Task: task}
	ff := t.forceForm(t.path()+"/task", []FormField{{"task", task}})
	ar, res, ok := callWrite(r, t, ops.OpSetTask, args, "Task", ff)
	if ok && res.Outcome == ops.OutcomeDelivered {
		// Delivered means the Worker stored the label; it sent no terminal input.
		ar.Summary = "Saved. The task is a label for this session; no terminal input was sent."
	}
	answerResultSignals(w, t, []ActionResult{ar}, resultEvents{taskSaved: ok && res.Outcome == ops.OutcomeDelivered}.withOutcome(ok, res))
}

func handleAnswer(w http.ResponseWriter, r *http.Request, t target) {
	// A quick-answer button posts "choice"; the answer box posts "choice_text".
	choice := strings.TrimSpace(r.PostFormValue("choice"))
	typed := strings.TrimSpace(r.PostFormValue("choice_text"))
	if choice != "" && typed != "" && choice != typed {
		// Two different answers: the page cannot tell which one the person meant.
		answerResult(w, t, []ActionResult{{Label: "Answer", Kind: "danger",
			Summary: "Choose one option or type an answer, not both. Nothing was sent."}}, false)
		return
	}
	if choice == "" {
		choice = typed
	}
	if choice == "" {
		answerResult(w, t, []ActionResult{{Label: "Answer", Kind: "danger", Summary: "Choose an answer first."}}, false)
		return
	}
	// The dialog the user saw. Without its tokens nothing is sent: the Worker
	// needs them, and the page must not invent them from the current screen.
	reviewed, ok := reviewedFromForm(r)
	if !ok {
		answerResultEvents(w, t, []ActionResult{{Label: "Answer", Kind: "danger",
			Summary: "The page lost the dialog it showed. Nothing was sent. Reload the page and answer again."}}, false, true)
		return
	}
	args := ops.AnswerArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7(), Force: forced(r)}, Target: t.opTarget(), Choice: choice, Expect: reviewed}
	// The binding is KEEP: a refused answer is never forceable, so there is no
	// "send anyway" form (force=1 is passed through for a client that sends it;
	// the Worker ignores it for the binding).
	ar, res, ok := callWrite(r, t, ops.OpAnswer, args, "Answer", nil)
	stale := false
	if ok && res.Outcome == ops.OutcomeFailedNotSent && (res.Reason == "pane_changed" || res.Reason == "dialog_changed") {
		stale = true
		ar = ActionResult{Label: "Answer", Kind: "danger", Reason: res.Reason,
			Summary: "The dialog changed since you looked at it. Nothing was sent. Read the dialog that is on the screen now and answer again."}
	}
	// A delivered answer tells the browser to release the typed answer (the box
	// is cleared when it still holds exactly that text).
	answerResultSignals(w, t, []ActionResult{ar}, resultEvents{sent: ok && res.Outcome == ops.OutcomeDelivered, stale: stale}.withOutcome(ok && !stale, res))
}

// newSessionArgs are the arguments of session_create as the REST route takes
// them: the Worker is named by the worker_id member of the request.
type newSessionArgs struct {
	ops.SessionCreateArgs
	WorkerID string `json:"worker_id"`
}

// handleNewSession creates a session (minion-N) on the Worker and opens it.
func handleNewSession(w http.ResponseWriter, r *http.Request, t target) {
	args := newSessionArgs{
		SessionCreateArgs: ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}},
		WorkerID:          t.worker.ID,
	}
	var res ops.WriteResult
	if err := t.c.CallOp(r, ops.OpSessionCreate, args, &res); err != nil {
		renderFragment(w, "result", ResultView{Results: []ActionResult{errorResult("New session", err)}})
		return
	}
	if res.Outcome != ops.OutcomeDelivered || res.Pane == nil || res.Pane.Session == "" {
		ar := resultOf("New session", res, nil)
		if res.Outcome == ops.OutcomeDelivered {
			ar = ActionResult{Label: "New session", Kind: "warning", Summary: "The session was created, but the Worker did not name it. Look for it in the session list."}
		}
		renderFragment(w, "result", ResultView{Results: []ActionResult{ar}})
		return
	}
	dest := SessionPath(t.ref, res.Pane.Session)
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func init() {
	RegisterPage(Page{Pattern: pathSessionPattern, Title: "Session", Template: "session.html", Data: sessionData, Embeddable: true})
	for _, rt := range []struct {
		pattern string
		fn      func(http.ResponseWriter, *http.Request, target)
	}{
		{"GET " + pathSessionPattern + "/screen", handlePanel},
		{"POST " + pathSessionPattern + "/text", handleSendText},
		{"POST " + pathSessionPattern + "/keys", handleSendKey},
		{"POST " + pathSessionPattern + "/task", handleSetTask},
		{"POST " + pathSessionPattern + "/answer", handleAnswer},
		{"POST " + pathNewSessionPattern, handleNewSession},
	} {
		RegisterRoute(Route{Pattern: rt.pattern, Handler: sessionRoute(rt.fn)})
	}
}
