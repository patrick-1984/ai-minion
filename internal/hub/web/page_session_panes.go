package web

// The right pane of the session workspace is a list of views. The workbench
// ships one, "Details" (owner, access, connection, identifiers). Other stories
// add theirs with [RegisterSessionPane]; the Conversation of Story 7.5 plugs in
// here without a change to the session page or its scripts: the pane shows its
// tab (when there are two or more views), its body and its own live updates.
//
// A pane renders its body with html/template (or any other escaping code) and
// returns it as template.HTML, so the page never inserts text it did not
// escape. A pane that fails or returns nothing is left out of the page.

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// SessionContext is what a pane may know about the session it is drawn for.
// It carries no secret and no terminal content.
type SessionContext struct {
	Worker   string
	WorkerID string
	Owner    string
	Session  string
	Online   bool
	// Operate is true when the caller may send input to the session.
	Operate bool
	Task    string
	// Kind is the session's kind from its snapshot (for example "agent:claude" or "shell"); empty when unknown.
	Kind string
	// InputKind is the strict kind that decides whether input is delivered;
	// empty when the Worker does not tell it apart from Kind.
	InputKind string
	// Ops and Features are what the Worker announced for this connection (empty when offline or not announced).
	Ops      []string
	Features []string
	// SessionCreated is the generation of the live session (creation time in
	// Unix seconds) from its snapshot; zero when unknown.
	SessionCreated int64
}

// SessionPane is one view of the right pane.
type SessionPane struct {
	// ID is a short stable identifier: lower-case letters, digits, "-" and "_".
	ID string
	// Label is the text of the view's tab.
	Label string
	// Order sorts the views, ascending; ties are broken by ID.
	Order int
	// Render returns the body of the view for one request. An empty result or an
	// error leaves the view out.
	Render func(r *http.Request, s SessionContext) (template.HTML, error)
}

// PaneView is a rendered pane of one page.
type PaneView struct {
	ID    string
	Label string
	HTML  template.HTML
}

var (
	paneMu    sync.Mutex
	paneList  []SessionPane
	paneIDRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	paneNames = template.Must(template.ParseFS(templateFiles, "templates/_icons.html", "templates/_session_panes.html"))
)

// RegisterSessionPane adds a view to the right pane of every session page. It
// panics for an invalid or duplicate registration; call it from an init
// function, like [RegisterPage].
func RegisterSessionPane(p SessionPane) {
	if !paneIDRe.MatchString(p.ID) || p.Label == "" || p.Render == nil {
		panic(fmt.Sprintf("web: invalid session pane %q", p.ID))
	}
	paneMu.Lock()
	defer paneMu.Unlock()
	for _, o := range paneList {
		if o.ID == p.ID {
			panic(fmt.Sprintf("web: session pane %q is registered twice", p.ID))
		}
	}
	paneList = append(paneList, p)
	sort.SliceStable(paneList, func(i, j int) bool {
		if paneList[i].Order != paneList[j].Order {
			return paneList[i].Order < paneList[j].Order
		}
		return paneList[i].ID < paneList[j].ID
	})
}

// renderSessionPanes renders every registered view for one request.
func renderSessionPanes(r *http.Request, t target, page SessionPage) []PaneView {
	paneMu.Lock()
	list := slices.Clone(paneList)
	paneMu.Unlock()
	sc := SessionContext{Worker: page.Worker, WorkerID: page.WorkerID, Owner: page.Owner, Session: page.Session,
		Online: page.Online, Operate: t.operate, Task: page.Task, Kind: page.Panel.Kind, InputKind: page.Panel.InputKind, Ops: t.worker.Ops, Features: t.worker.Features,
		SessionCreated: page.Panel.SessionCreated}
	var out []PaneView
	for _, p := range list {
		body, err := p.Render(r, sc)
		if err != nil || body == "" {
			continue
		}
		out = append(out, PaneView{ID: p.ID, Label: p.Label, HTML: body})
	}
	return out
}

// detailsData is what the "session_details" template sees.
type detailsData struct {
	SessionContext
	Kind    string
	Version string
	// Usage holds what the Hub knows about the harness of the current run of
	// the session (from the Worker's usage report); empty when it knows
	// nothing, and UsageNote then says why.
	Usage     []DiagnosticsFact
	UsageNote string
}

// sessionUsageFacts lists what the usage report says about the live run of
// the session, the one named session that started at created (the identity
// sessions_list gives; a reused name is another run): the harness and its
// version, the model, how it is paid for, the last report and the token
// totals when known. An ended row or one of an earlier run never matches. A
// value the report does not carry is left out (never shown as zero).
//
// The run identity the snapshot and the usage row share is the name and the
// creation second, which a session recreated within the same second also
// has. A row is therefore taken only when the Worker last listed it live
// after that second ended: by then a session of that name created in that
// second and still alive can only be the current one (an earlier one of that
// second was already gone, and a complete listing without it ended its
// row). More than one such row is ambiguous and shows nothing.
func sessionUsageFacts(rows []ops.UsageSessionRow, session string, created time.Time) []DiagnosticsFact {
	if created.IsZero() {
		return nil
	}
	settled := (created.Unix() + 1) * 1000
	var best *ops.UsageSessionRow
	for i := range rows {
		r := &rows[i]
		if r.Name != session || r.StoppedAt != nil || r.StartedAt == nil || *r.StartedAt/1000 != created.Unix() {
			continue
		}
		if r.LastSeen < settled {
			continue
		}
		if best != nil {
			return nil
		}
		best = r
	}
	if best == nil {
		return nil
	}
	var out []DiagnosticsFact
	add := func(l, v string) {
		if v != "" {
			out = append(out, DiagnosticsFact{l, v})
		}
	}
	h := best.Harness
	if best.Version != "" && h != "" {
		h += " " + best.Version
	}
	add("Harness", h)
	add("Model", best.Model)
	add("Access", best.AuthMode)
	if best.ObservedAt > 0 {
		ts := ops.Timestamp(time.UnixMilli(best.ObservedAt))
		add("Usage last reported", formatTime(&ts, ""))
	}
	num := func(l string, v *int64) {
		if v != nil {
			add(l, strconv.FormatInt(*v, 10))
		}
	}
	num("Input tokens", best.InputTokens)
	num("Output tokens", best.OutputTokens)
	num("Context tokens", best.ContextTokens)
	if best.CostUSD != nil {
		add("Reported cost", "$"+strconv.FormatFloat(*best.CostUSD, 'f', 4, 64))
	}
	return out
}

// liveSessionUsage finds the usage of the live run of the session: the run
// its snapshot names (the session generation), looked up directly among the
// live usage rows of that session on the Worker. Without one certain match it
// says usage is unavailable.
func liveSessionUsage(r *http.Request, c OpCaller, s SessionContext) ([]DiagnosticsFact, string) {
	const unavailable = "Unavailable: no usage report for the current run of this session"
	if !s.Online {
		return nil, "Unavailable while the Worker is offline"
	}
	if s.SessionCreated <= 0 {
		return nil, unavailable
	}
	var res ops.UsageListResult
	if err := c.CallOp(r, ops.OpUsageList, ops.UsageListArgs{Section: "sessions", WorkerID: s.WorkerID, LiveSession: s.Session, Limit: 200}, &res); err != nil || res.Next != "" {
		return nil, unavailable
	}
	if f := sessionUsageFacts(res.Sessions, s.Session, time.Unix(s.SessionCreated, 0)); f != nil {
		return f, ""
	}
	return nil, unavailable
}

func renderDetails(r *http.Request, s SessionContext) (template.HTML, error) {
	d := detailsData{SessionContext: s}
	if c, err := sessionCaller(); err == nil {
		if w, _, _, err := resolveWorker(r, c); err == nil {
			d.Kind, d.Version = w.Kind, w.ProtoVersion
		}
		d.Usage, d.UsageNote = liveSessionUsage(r, c, s)
	}
	var buf bytes.Buffer
	if err := paneNames.ExecuteTemplate(&buf, "session_details", d); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil //nolint:gosec // produced by html/template above
}

func init() {
	RegisterSessionPane(SessionPane{ID: "details", Label: "Details", Order: 100, Render: renderDetails})
}
