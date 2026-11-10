package web

// Story 7.5: the pipe conversation of a session. One component, rendered once per
// document, shows the stored pipe turns of the session (messages sent to the agent and
// the replies the Worker could extract with certainty) and sends one message at a time
// through the Hub's pipe broker (pipe_send). Nothing here reads the screen to make up a
// conversation: every entry is a stored turn the broker returned from pipe_read, and a
// send is the one guarded pipe_send call with its own outcome, trust, budget and "send
// anyway" flow. The text of a turn is escaped by html/template and is never markup.
//
// Where it appears: in the full page it is the "Conversation" view of the right pane
// (the session pane registry); in the compact inspector view (?embed=1), which has no
// pane, the same rendered HTML is placed in the input dock as the Conversation tab (or,
// for a person who may only read, as a section under the screen). The page template
// chooses the place; the component and its ids exist exactly once either way.

import (
	"bytes"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const (
	pathPipeSuffix = "/pipe"
	// pathPipeTurns is the read route of the turn list; it must never end in "/pipe" (the page treats that as the write).
	pathPipeTurns = "/pipe/turns"
	// pipePageLimit is the page size of one pipe_read; pipeMaxPages bounds the pages followed
	// for one request (the work and the memory of one answer), and pipeShownTurns is how many of the newest turns the
	// view shows. A history longer than pipeMaxPages pages is not cut off: the answer says it is behind and names the
	// cursor to go on from (see readPipe), and the browser asks again until the newest turn is reached.
	pipePageLimit  = ops.MaxPipeReadLimit
	pipeMaxPages   = 5
	pipeShownTurns = 200
	// maxPipeTextRunes bounds the text of one turn the view renders.
	maxPipeTextRunes = 8192
)

// PipeTurnView is one entry of the conversation as the template sees it.
type PipeTurnView struct {
	ID        string
	Direction string // ops.PipeSent or ops.PipeReceived
	Who       string
	Text      string
	At        string
	AtISO     string
}

// PipePanel is what the "pipe_panel" and "pipe_turns" templates see.
type PipePanel struct {
	Session string
	Worker  string
	Target  string
	// ReadURL and SendURL are the canonical routes of this session.
	ReadURL string
	SendURL string
	// Operate: the caller may send input (the composer is drawn at all).
	Operate bool
	// Sendable: the composer is enabled. Unavailable says why it is not, in words that only
	// state what is known (the Worker is offline, does not announce the pipe, or the session is a shell).
	Sendable    bool
	Unavailable string
	Turns       []PipeTurnView
	// Latest is the id of the newest shown turn ("" when there is none).
	Latest string
	// Truncated: older turns exist than are shown.
	Truncated bool
	// Behind: the turns shown are NOT yet the newest ones; the read stopped at its page bound before the end of the
	// history. Walk is then the cursor the browser sends back to go on; when Behind is false it is the cursor
	// that makes the next read cheap (it reads exactly the shown turns and whatever is newer), or "".
	Behind bool
	Walk   string
	// ReadError is shown instead of the turns when the broker refused or failed the read.
	ReadError string
	// Withheld and Notice come from the broker when it withholds turns of an untrusted source.
	Withheld int
	Notice   string
}

var pipeTemplates = sync.OnceValue(func() *template.Template {
	return template.Must(template.ParseFS(templateFiles, "templates/_icons.html", "templates/_pipe_panel.html"))
})

// pipeAvailability says whether the composer may be used and, if not, the known reason. It reads only what
// the Hub knows: the Worker's announced operations and safeguards, its connection, and the session's kind.
func pipeAvailability(s SessionContext) (sendable bool, why string) {
	switch {
	case !s.Online:
		return false, "The Worker is offline, so a message cannot be sent now. Earlier messages stay readable here; sending comes back when it reconnects."
	case !s.Operate:
		return false, ""
	case len(s.Ops) > 0 && !containsString(s.Ops, ops.OpPipeSend):
		return false, "This Worker does not offer the pipe (it did not announce pipe_send), so messages cannot be sent from here."
	}
	for _, f := range ops.RequiredFeatures(ops.OpPipeSend, []byte(`{"text":"x"}`)) {
		if len(s.Features) > 0 && !containsString(s.Features, f) {
			return false, "This Worker needs an update before it can use the pipe (it did not announce " + f + "). Nothing can be sent from here."
		}
	}
	if s.Kind == ops.PaneKindShell {
		return false, "This is a shell session, not an agent session. The pipe delivers messages to agent sessions only; use Text to type into a shell."
	}
	return true, ""
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// pipeRead is the result of one bounded read of the pipe.
type pipeRead struct {
	Turns []ops.PipeTurn // the newest at most pipeShownTurns turns of the walk, in commit order
	// Older: an entry before Turns exists (and is not shown).
	Older bool
	// Behind: the walk stopped at the page bound; Turns are the newest it saw, not the newest there are.
	Behind bool
	// Walk: with Behind, the cursor to go on from; otherwise the cursor that reads exactly Turns and what is
	// newer ("" when Turns start at the beginning of the pipe).
	Walk     string
	Withheld int
	Notice   string
}

// readPipe reads the pipe of tg through the broker's pipe_read, following the cursor for at most pipeMaxPages pages
// from the cursor from ("" is the start of the pipe), so the work and the memory of one call are bounded and the
// broker authorises every page: a page that fails ends the call with its error and nothing of the earlier pages is
// returned. pipe_read only goes forward from a cursor, so a longer history is read in steps: when the bound is
// reached the answer is Behind and carries the cursor of the next step, chosen so that the next step still has
// the newest turns of this one (no gap) and always moves forward. The browser keeps asking until the answer is
// not Behind: the newest turns are then shown. Nothing is dropped silently: Older and Behind say what is not shown.
func readPipe(r *http.Request, c OpCaller, tg ops.Target, from string) (pipeRead, error) {
	out, err := readPipeFrom(r, c, tg, from)
	if err == nil && from != "" && len(out.Turns) == 0 && out.Withheld == 0 {
		// Nothing follows the cursor: it is the newest turn (or stale). Never answer with an empty conversation that
		// would replace the shown one: read again from the start (bounded, authorised afresh).
		return readPipeFrom(r, c, tg, "")
	}
	var oe *OpError
	if err != nil && from != "" && errors.As(err, &oe) && (oe.Status == http.StatusNotFound || oe.Status == http.StatusBadRequest || oe.Status == http.StatusUnprocessableEntity || oe.Status == http.StatusConflict) {
		// The cursor of an earlier step is gone (retention) or never was: start again from the beginning. A real
		// refusal (the session, the person's access) fails the same way again and is returned.
		return readPipeFrom(r, c, tg, "")
	}
	return out, err
}

func readPipeFrom(r *http.Request, c OpCaller, tg ops.Target, from string) (pipeRead, error) {
	var out pipeRead
	after, anchor := from, from
	var win []ops.PipeTurn
	ended := false
	for page := 0; page < pipeMaxPages; page++ {
		var res ops.PipeReadResult
		if err := c.CallOp(r, ops.OpPipeRead, ops.PipeReadArgs{Target: tg, AfterTurnID: after, Limit: pipePageLimit}, &res); err != nil {
			return pipeRead{}, err
		}
		if res.Withheld > 0 {
			out.Withheld += res.Withheld
			out.Notice = res.Notice
		}
		if len(res.Turns) == 0 {
			ended = true
			break
		}
		win = append(win, res.Turns...)
		after = res.Turns[len(res.Turns)-1].TurnID
		if len(win) > pipeShownTurns {
			anchor = win[len(win)-pipeShownTurns-1].TurnID
			win = append([]ops.PipeTurn(nil), win[len(win)-pipeShownTurns:]...)
		}
	}
	out.Turns, out.Older, out.Behind = win, anchor != "", !ended
	switch {
	case !ended && anchor != from:
		out.Walk = anchor // the next step starts at the turn before the shown ones
	case !ended:
		// Fewer than a window was read (small, byte-limited pages): move on by half of what was read, so the next step
		// still reads the newest turns of this one. If it reaches the end without anything newer it shows that
		// overlap and never an empty list; the walk still moves forward (every page read is non-empty, so at
		// least pipeMaxPages turns were read and half of them is at least 2).
		out.Walk = win[len(win)-1-len(win)/2].TurnID
	default:
		out.Walk = anchor
	}
	return out, nil
}

func pipeTurnView(t ops.PipeTurn) PipeTurnView {
	v := PipeTurnView{ID: t.TurnID, Direction: t.Direction, Text: truncateRunes(t.Text, maxPipeTextRunes)}
	if t.Direction == ops.PipeSent {
		// The label does not claim a delivery: the stored message does not carry the outcome of its delivery (a message
		// whose outcome was unknown or partial is stored too). The outcome is what the send itself reported.
		v.Who = "Your message to the agent"
	} else {
		v.Direction = ops.PipeReceived
		v.Who = "Agent reply"
	}
	if tm := time.Time(t.At); !tm.IsZero() {
		v.At = tm.UTC().Format("15:04:05") + " UTC"
		v.AtISO = tm.UTC().Format(time.RFC3339)
	}
	return v
}

// pipePanelFor builds the component data of one session: availability and the stored turns.
func pipePanelFor(r *http.Request, c OpCaller, s SessionContext, path, from string) PipePanel {
	p := PipePanel{
		Session: s.Session, Worker: s.Worker, Target: s.Session + " on " + s.Worker,
		ReadURL: path + pathPipeTurns, SendURL: path + pathPipeSuffix, Operate: s.Operate,
	}
	p.Sendable, p.Unavailable = pipeAvailability(s)
	rd, err := readPipe(r, c, ops.Target{WorkerID: s.WorkerID, SessionName: s.Session}, from)
	if err != nil {
		p.ReadError = "The conversation could not be read just now. " + truncateRunes(failureText(err), 200)
		return p
	}
	for _, t := range rd.Turns {
		p.Turns = append(p.Turns, pipeTurnView(t))
	}
	if len(p.Turns) > 0 {
		p.Latest = p.Turns[len(p.Turns)-1].ID
	}
	p.Truncated, p.Behind, p.Walk, p.Withheld, p.Notice = rd.Older, rd.Behind, rd.Walk, rd.Withheld, rd.Notice
	return p
}

func renderPipe(name string, p PipePanel) (template.HTML, error) {
	var buf bytes.Buffer
	if err := pipeTemplates().ExecuteTemplate(&buf, name, p); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil //nolint:gosec // produced by html/template above
}

// renderConversation is the session pane of the conversation (and, in the compact view, the same HTML).
func renderConversation(r *http.Request, s SessionContext) (template.HTML, error) {
	c, err := sessionCaller()
	if err != nil {
		return "", err
	}
	return renderPipe("pipe_panel", pipePanelFor(r, c, s, SessionPath(r.PathValue("worker"), s.Session), ""))
}

// sessionContextOf is the SessionContext of a resolved target (the live routes need the same facts as the page).
func sessionContextOf(t target) SessionContext {
	return SessionContext{Worker: t.worker.Name, WorkerID: t.worker.ID, Owner: t.worker.Owner, Session: t.session,
		Online: t.worker.Online, Operate: t.operate, Ops: t.worker.Ops, Features: t.worker.Features}
}

// handlePipeTurns answers the turn list that live refresh swaps in. It reads only: a refused or failed read is
// an error status and the browser keeps the list it has.
func handlePipeTurns(w http.ResponseWriter, r *http.Request, t target) {
	from := r.URL.Query().Get("walk") // the cursor a step of a long history goes on from (see readPipe)
	if !ops.ValidUUIDv7(from) {
		from = ""
	}
	p := pipePanelFor(r, t.c, sessionContextOf(t), t.path(), from)
	if p.ReadError != "" {
		http.Error(w, p.ReadError, http.StatusBadGateway)
		return
	}
	html, err := renderPipe("pipe_turns", p)
	if err != nil {
		http.Error(w, "the conversation could not be rendered", http.StatusInternalServerError)
		return
	}
	writeFragment(w, []byte(html))
}

// handlePipeSend sends one message through pipe_send. A blank message is refused before any call. A refused
// call that the broker says can be forced is repeated only by the person's explicit confirmation, as the same
// single call with the original text; an unknown outcome is kept by the page and never sent again.
func handlePipeSend(w http.ResponseWriter, r *http.Request, t target) {
	text := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(r.PostFormValue("text"))
	if blankText(text) {
		answerResult(w, t, []ActionResult{{Label: "Message", Kind: "danger", Summary: "Type a message first. Nothing was sent."}}, false)
		return
	}
	ff := t.forceForm(t.path()+pathPipeSuffix, []FormField{{"text", text}})
	args := ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7(), Force: forced(r)}, Target: t.opTarget(), Text: text}
	ar, res, ok := callWrite(r, t, ops.OpPipeSend, args, "Message", ff)
	if ok && (res.Outcome == ops.OutcomeUnknown || res.Outcome == ops.OutcomePartial) {
		ar.Submitted = truncateRunes(text, maxSubmittedRunes)
	}
	ev := resultEvents{sent: ok && res.Outcome == ops.OutcomeDelivered, pipe: true}.withOutcome(ok, res)
	answerResultSignals(w, t, []ActionResult{ar}, ev)
}

func init() {
	RegisterSessionPane(SessionPane{ID: "conversation", Label: "Conversation", Order: 50, Render: renderConversation})
	RegisterRoute(Route{Pattern: "GET " + pathSessionPattern + pathPipeTurns, Handler: sessionRoute(handlePipeTurns)})
	RegisterRoute(Route{Pattern: "POST " + pathSessionPattern + pathPipeSuffix, Handler: sessionRoute(handlePipeSend)})
}
