package web

// The Hidden sessions panel of the Worker detail page (v0.23.0): the owner of
// a Worker or an administrator (on any Worker, the main and infrastructure
// Workers of other users included) allows or refuses new sessions that would
// be hidden from the person's terminal on this Worker, or returns it to each
// caller's account-wide preference (worker_hidden_sessions_set). The Hub
// setting hidden_sessions still decides for everybody when it is allowed or
// disabled; the panel says so. Every change is audited by the Hub.

import (
	"html/template"
	"net/http"
	"net/url"
	"sync"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

const pathWorkerHiddenPattern = "/w/{worker}/hidden-sessions"

// WorkerHiddenPath is the Hidden sessions panel of a Worker: GET renders it,
// POST (CSRF-checked, hidden_sessions=allow|refuse|default) changes it.
func WorkerHiddenPath(worker string) string {
	return "/w/" + url.PathEscape(worker) + "/hidden-sessions"
}

// WorkerHiddenView is what the "worker_hidden_panel" fragment sees.
type WorkerHiddenView struct {
	WorkerID string
	Worker   string
	URL      string
	// Choice is the Worker's own choice: allow, refuse or default.
	Choice string
	// Operate is whether this person may change it (owner or administrator).
	Operate bool
	// SessionsHidden is true when the connected Worker reports that its new
	// sessions would be hidden; Online whether it is connected now.
	SessionsHidden bool
	Online         bool
	// HubNote says that the Hub setting decides for everybody now; Default
	// describes the caller's own account-wide preference.
	HubNote string
	Default string
	Results []ActionResult
}

var workerHiddenTemplates = sync.OnceValue(func() *template.Template {
	return template.Must(template.ParseFS(templateFiles, "templates/worker_hidden.html"))
})

// workerHiddenChoice is the panel value of a Worker's stored choice.
func workerHiddenChoice(w ops.WorkerInfo) string {
	switch {
	case w.AllowHiddenSessions == nil:
		return ops.WorkerHiddenDefault
	case *w.AllowHiddenSessions:
		return ops.WorkerHiddenAllow
	}
	return ops.WorkerHiddenRefuse
}

func workerHiddenView(r *http.Request, t target) WorkerHiddenView {
	v := WorkerHiddenView{WorkerID: t.worker.ID, Worker: t.worker.Name, URL: WorkerHiddenPath(t.worker.ID),
		Choice: workerHiddenChoice(t.worker), Operate: t.operate, SessionsHidden: t.worker.SessionsHidden, Online: t.worker.Online}
	if v.Worker == "" {
		v.Worker = t.worker.ID
	}
	var prefs ops.AccountPrefsResult
	if err := t.c.CallOp(r, ops.OpAccountPrefs, ops.AccountPrefsArgs{}, &prefs); err == nil {
		switch prefs.HiddenSessions {
		case config.HiddenSessionsAllowed:
			v.HubNote = "An administrator allows hidden sessions for everybody on this Hub (Hub setting hidden_sessions), so this choice has no effect now."
		case config.HiddenSessionsDisabled:
			v.HubNote = "An administrator has disabled hidden sessions for everybody on this Hub (Hub setting hidden_sessions), so this choice has no effect now."
		}
		if prefs.AllowHiddenSessions {
			v.Default = "allowed by your account setting"
		} else {
			v.Default = "refused by your account setting"
		}
	}
	return v
}

func handleWorkerHiddenGet(w http.ResponseWriter, r *http.Request, t target) {
	writeFragmentTemplate(w, workerHiddenTemplates(), "worker_hidden_panel", workerHiddenView(r, t))
}

func handleWorkerHiddenSet(w http.ResponseWriter, r *http.Request, t target) {
	choice := r.PostForm.Get("hidden_sessions")
	if !t.operate {
		v := workerHiddenView(r, t)
		v.Results = []ActionResult{{Label: "Hidden sessions", Kind: "danger", Summary: "Only the owner of this worker, or an administrator, can change this."}}
		writeFragmentTemplate(w, workerHiddenTemplates(), "worker_hidden_panel", v)
		return
	}
	args := ops.WorkerHiddenSessionsSetArgs{WriteArgs: newWriteArgs(), WorkerID: t.worker.ID, HiddenSessions: choice}
	var res ops.WriteResult
	ar := ActionResult{Label: "Hidden sessions", Kind: "info"}
	if err := t.c.CallOp(r, ops.OpWorkerHiddenSessionsSet, args, &res); err != nil {
		ar.Kind, ar.Summary = "danger", failureText(err)
	} else {
		switch choice {
		case ops.WorkerHiddenAllow:
			ar.Summary = "New sessions that would be hidden are allowed on this worker."
		case ops.WorkerHiddenRefuse:
			ar.Summary = "New sessions that would be hidden are refused on this worker."
		default:
			ar.Summary = "This worker follows each person's account setting again."
		}
		for _, wn := range res.Warnings {
			ar.Warnings = append(ar.Warnings, wn.Message)
		}
		// Show the stored value: read the Worker again.
		if fresh, _, operate, err := resolveWorker(r, t.c); err == nil {
			t.worker, t.operate = fresh, operate
		}
	}
	v := workerHiddenView(r, t)
	v.Results = []ActionResult{ar}
	writeFragmentTemplate(w, workerHiddenTemplates(), "worker_hidden_panel", v)
}

func init() {
	RegisterRoute(Route{Pattern: "GET " + pathWorkerHiddenPattern, Handler: sessionRoute(handleWorkerHiddenGet)})
	RegisterRoute(Route{Pattern: "POST " + pathWorkerHiddenPattern, Handler: sessionRoute(handleWorkerHiddenSet)})
}
