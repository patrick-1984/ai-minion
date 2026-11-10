package web

// The Worker detail page (Story 5.7): /workers/<id>. A Worker name in any list is a link
// to this page, so that the Worker is an entity with its own view, as a session is. The
// page is a guarded READ: it shows nothing the lists would not, and decides nothing.
//
//   - The Worker is addressed by its canonical id. A display name is not unique (two owners
//     may have a Worker of the same name) and never addresses a Worker here.
//   - Everything shown comes from the same authorised snapshot as the Workers and Sessions
//     lists (the Hub's own REST read as the signed-in user, in the scope of the request:
//     Mine, or All users for an administrator who acts as themselves). A Worker that is not
//     in that snapshot, because it does not exist, belongs to someone else, or belongs to
//     someone else's scope, is the same plain 404 as an unknown id: nothing leaks, and an
//     old cookie, another account or another scope cannot read it.
//   - The New session button is the existing route (POST /w/<id>/sessions, the same CSRF
//     and operation checks) and is offered only when this person may operate the Worker
//     (an administrator, or its owner; never while acting as someone else who is not) and
//     the Worker is online. The page hides what the API would refuse; the API still decides.
//   - The sessions table is the sessions list, narrowed to this Worker, with the same row
//     behaviour: the session name, or anywhere else in the row, opens the session inspector.
//   - Nothing is invented: no host name, version, uptime or capability that the Hub does not
//     report. The page lists the name, the owner (when the scope shows owners), the kind, the
//     status, the protocol the Worker negotiated and its sessions.
//
// The page updates live through its own stream (/live/workers/<id>), which renders the same
// fragment from a fresh snapshot of the stream's own request, so a Worker that goes offline,
// a session that appears and a Worker that leaves the list show without a reload.

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Paths of the Worker detail page.
const (
	pathWorkerPattern     = "/workers/{worker}"
	pathLiveWorkerPattern = "/live/workers/{worker}"
)

// WorkerPath returns the path of the detail page of a Worker, by canonical id. With all set
// the link keeps the All users scope, without which an administrator would land in Mine,
// where another owner's Worker is (correctly) not found.
func WorkerPath(id string, all bool) string {
	return scopedLink("/workers/"+url.PathEscape(id), all)
}

// WorkerView is what the "worker_results" fragment sees: one Worker and its sessions.
type WorkerView struct {
	Worker WorkerRow
	// Sessions are the Worker's sessions of the snapshot, with the same fields as the
	// session list (HideWorker is set).
	Sessions []SessionRow
	// ShowOwner is true in the All users scope (the session table then has an Owner column).
	ShowOwner bool
	// OnWorker tells the session table that every row is of this Worker.
	OnWorker bool
	// Operate is whether this person may operate the Worker (the New session button).
	Operate bool
	// Gone is set when the Worker is not in the snapshot (it left the list while the page
	// was open): the fragment then says so instead of showing stale facts.
	Gone         bool
	Notice       string
	SessionsText string
	// SessionsURL is the session list narrowed to this Worker, in the same scope.
	SessionsURL string
}

// WorkerPage is what worker.html sees as .Data.
type WorkerPage struct {
	WorkerView
	Scope     string
	CanFilter bool
	MineURL   string
	AllURL    string
	// ListURL is the Workers list in the same scope; LiveURL the stream of this page.
	ListURL string
	LiveURL string
	// ShareURL is the share panel of the Worker (Story 4.15); empty when this person
	// may not operate the Worker, so the page offers nothing the API would refuse.
	ShareURL string
	// Name is the heading (the Worker's name; the id when it has none).
	Name    string
	Context string
}

// Crumbs implements [Crumbed]: a Worker lives below the Workers list.
func (p WorkerPage) Crumbs() []Crumb {
	return []Crumb{{Label: "Control center", Path: "/"}, {Label: "Workers", Path: p.ListURL}, {Label: p.Name}}
}

// newWorkerView builds the fragment data of one snapshot. It never invents: a Worker that the
// snapshot does not list is Gone.
func newWorkerView(d Dashboard, all bool, id string, operate bool) WorkerView {
	v := newDashboardView(d, all)
	out := WorkerView{ShowOwner: all, OnWorker: true, Notice: d.Notice}
	i := slices.IndexFunc(v.Workers, func(w WorkerRow) bool { return w.ID == id })
	if i < 0 {
		out.Gone = true
		return out
	}
	out.Worker = v.Workers[i]
	out.Worker.Sessions = nil
	// The action addresses the Worker by its canonical id (a name is not unique); the route resolves an id like a name.
	out.Worker.NewURL = NewSessionPath(id)
	for _, x := range v.Sessions {
		if x.WorkerID == id {
			x.HideWorker = true
			out.Sessions = append(out.Sessions, x)
		}
	}
	out.Operate = operate && out.Worker.Online
	out.SessionsText = countText(len(out.Sessions), len(out.Sessions), "session", "sessions")
	out.SessionsURL = out.Worker.SessionsURL
	return out
}

// workerOperate is whether the request's user may operate the Worker: the base permission of
// AD-11, as the session page computes it (an administrator operates every Worker, the owner
// the own ones; while an administrator acts as another user the API treats the session as
// that user, so the page does too). A failure is "no": the button is only a convenience.
func workerOperate(r *http.Request, id string) bool {
	c, err := sessionCaller()
	if err != nil {
		return false
	}
	uid, admin, ok := requestUser(r)
	if !ok {
		return false
	}
	var res ops.WorkersListResult
	if err := c.CallOp(r, ops.OpWorkersList, ops.WorkersListArgs{All: admin}, &res); err != nil {
		return false
	}
	for _, w := range res.Workers {
		if w.ID == id {
			return admin || w.OwnerID == uid
		}
	}
	return false
}

func workerData(get func() LiveSource) func(*http.Request) (any, error) {
	return func(r *http.Request) (any, error) {
		src := get()
		if src == nil {
			return nil, &StatusError{Code: http.StatusServiceUnavailable, Message: "This page is not connected to the Hub yet."}
		}
		all, canFilter := dashboardScope(r)
		d, err := src.Snapshot(r, all)
		if err != nil {
			return nil, pageError(err)
		}
		id := r.PathValue("worker")
		operate := workerOperate(r, id)
		v := newWorkerView(d, all, id, operate)
		if v.Gone {
			return nil, ErrNotFound() // the same answer for an unknown id, another owner's Worker and another scope
		}
		own := "/workers/" + url.PathEscape(id)
		p := WorkerPage{WorkerView: v, Scope: scopeMine, CanFilter: canFilter, MineURL: own, AllURL: own + "?scope=all",
			ListURL: scopedLink(pathWorkers, all), LiveURL: "/live/workers/" + url.PathEscape(id), Name: v.Worker.Name, Context: principalContext(r)}
		if p.Name == "" {
			p.Name = id
		}
		if operate {
			p.ShareURL = SharePath(id)
		}
		if all {
			p.Scope = scopeAll
			p.LiveURL += "?scope=all"
		}
		return p, nil
	}
}

// renderWorkerFragment renders the fragment of one snapshot for the live stream.
func renderWorkerFragment(v WorkerView) (string, error) {
	var buf bytes.Buffer
	if err := fragmentTemplates.ExecuteTemplate(&buf, "worker_results", v); err != nil {
		return "", fmt.Errorf("web: render the worker fragment: %w", err)
	}
	return buf.String(), nil
}

func workerLiveHandler(get func() LiveSource, log *slog.Logger) *liveHandler {
	h := registeredLiveHandler(get, log)
	h.render = func(d Dashboard, all bool, r *http.Request) (string, error) {
		id := r.PathValue("worker")
		return renderWorkerFragment(newWorkerView(d, all, id, workerOperate(r, id)))
	}
	return h
}

// registerWorkerPage adds the Worker detail page and its stream to reg.
func registerWorkerPage(reg *Registry, get func() LiveSource, log *slog.Logger) error {
	if err := reg.RegisterPage(Page{Pattern: pathWorkerPattern, Title: "Worker", Template: "worker.html", Inspector: true, Data: workerData(get)}); err != nil {
		return err
	}
	return reg.RegisterRoute(Route{Pattern: "GET " + pathLiveWorkerPattern, Handler: workerLiveHandler(get, log)})
}
