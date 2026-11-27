package web

// The App updates panel of the Worker detail page (v0.24.0): an
// administrator chooses how the Hub updates an Android Worker (the Minion
// app): Automatic (the Hub sends its release by itself; the app installs it
// when no client is attached and no agent is busy), Manual (the Update button
// only, the default) or Off (the Hub refuses to update it). The owner's
// Updates setting in the app still wins. Every change is audited by the Hub
// (worker_app_update_set).

import (
	"html/template"
	"net/http"
	"net/url"
	"sync"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const pathWorkerAppUpdatePattern = "/w/{worker}/app-update"

// WorkerAppUpdatePath is the App updates panel of a Worker: GET renders it,
// POST (CSRF-checked, policy=auto|manual|off) changes it.
func WorkerAppUpdatePath(worker string) string {
	return "/w/" + url.PathEscape(worker) + "/app-update"
}

// WorkerAppUpdateView is what the "worker_app_update_panel" fragment sees.
type WorkerAppUpdateView struct {
	WorkerID string
	Worker   string
	URL      string
	// Policy is auto, manual or off.
	Policy string
	// Admin is whether this person may change it.
	Admin   bool
	Results []ActionResult
}

var workerAppUpdateTemplates = sync.OnceValue(func() *template.Template {
	return template.Must(template.ParseFS(templateFiles, "templates/worker_app_update.html"))
})

// workerAppUpdatePolicy is the panel value of a Worker's stored policy.
func workerAppUpdatePolicy(w ops.WorkerInfo) string {
	if w.AppUpdate == "" {
		return ops.WorkerAppUpdateManual
	}
	return w.AppUpdate
}

func workerAppUpdateView(r *http.Request, t target) WorkerAppUpdateView {
	v := WorkerAppUpdateView{WorkerID: t.worker.ID, Worker: t.worker.Name, URL: WorkerAppUpdatePath(t.worker.ID),
		Policy: workerAppUpdatePolicy(t.worker), Admin: isAdminRequest(r)}
	if v.Worker == "" {
		v.Worker = t.worker.ID
	}
	return v
}

func handleWorkerAppUpdateGet(w http.ResponseWriter, r *http.Request, t target) {
	writeFragmentTemplate(w, workerAppUpdateTemplates(), "worker_app_update_panel", workerAppUpdateView(r, t))
}

func handleWorkerAppUpdateSet(w http.ResponseWriter, r *http.Request, t target) {
	policy := r.PostForm.Get("policy")
	if !isAdminRequest(r) {
		v := workerAppUpdateView(r, t)
		v.Results = []ActionResult{{Label: "App updates", Kind: "danger", Summary: "Only an administrator can change how the Hub updates this worker."}}
		writeFragmentTemplate(w, workerAppUpdateTemplates(), "worker_app_update_panel", v)
		return
	}
	args := ops.WorkerAppUpdateSetArgs{WriteArgs: newWriteArgs(), WorkerID: t.worker.ID, Policy: policy}
	var res ops.WriteResult
	ar := ActionResult{Label: "App updates", Kind: "info"}
	if err := t.c.CallOp(r, ops.OpWorkerAppUpdateSet, args, &res); err != nil {
		ar.Kind, ar.Summary = "danger", failureText(err)
	} else {
		switch policy {
		case ops.WorkerAppUpdateAuto:
			ar.Summary = "The Hub updates this worker by itself when a new release is out; the app installs it when no client is attached and no agent is busy."
		case ops.WorkerAppUpdateOff:
			ar.Summary = "The Hub does not update this worker; the Update button is gone."
		default:
			ar.Summary = "This worker is updated only when an administrator presses Update."
		}
		if fresh, _, operate, err := resolveWorker(r, t.c); err == nil {
			t.worker, t.operate = fresh, operate
		}
	}
	v := workerAppUpdateView(r, t)
	v.Results = []ActionResult{ar}
	writeFragmentTemplate(w, workerAppUpdateTemplates(), "worker_app_update_panel", v)
}

func init() {
	RegisterRoute(Route{Pattern: "GET " + pathWorkerAppUpdatePattern, Handler: sessionRoute(handleWorkerAppUpdateGet)})
	RegisterRoute(Route{Pattern: "POST " + pathWorkerAppUpdatePattern, Handler: sessionRoute(handleWorkerAppUpdateSet)})
}
