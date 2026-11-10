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
	"sync"
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
	// Ops and Features are what the Worker announced for this connection (empty when offline or not announced).
	Ops      []string
	Features []string
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
		Online: page.Online, Operate: t.operate, Task: page.Task, Kind: page.Panel.Kind, Ops: t.worker.Ops, Features: t.worker.Features}
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
}

func renderDetails(r *http.Request, s SessionContext) (template.HTML, error) {
	d := detailsData{SessionContext: s}
	if c, err := sessionCaller(); err == nil {
		if w, _, _, err := resolveWorker(r, c); err == nil {
			d.Kind, d.Version = w.Kind, w.ProtoVersion
		}
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
