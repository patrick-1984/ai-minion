package web

// The "Install an app from Minion files" panel of the Worker detail page
// (app_install, v0.25.0): for the owner of an Android Worker and
// administrators. It lists the .apk files at the top of the Worker's Minion
// files folder (app_install_status), with an Install button each, and shows
// the outcome of the last install. The panel renders nothing for a Worker
// that does not announce app.install.v1. The Hub's authorization of
// app_install decides again on every call.

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

const pathWorkerAppsPattern = "/w/{worker}/apps"

// WorkerAppsPath is the app install panel of a Worker: GET renders it, POST
// (CSRF-checked) installs one APK.
func WorkerAppsPath(worker string) string {
	return "/w/" + url.PathEscape(worker) + "/apps"
}

// WorkerAppsView is what the "worker_apps_panel" fragment sees.
type WorkerAppsView struct {
	// Show is false when the Worker cannot install apps or the person may
	// not: the panel is left out.
	Show     bool
	WorkerID string
	Worker   string
	URL      string
	Status   ops.AppInstallStatus
	// Last is the outcome of the last attempt, in words; empty without one.
	Last    *AppOutcomeView
	Error   string
	Results []ActionResult
}

// AppOutcomeView is one install outcome as the panel shows it.
type AppOutcomeView struct {
	ops.AppInstallResult
	// Text is the outcome in words.
	Text string
	// Kind is the flash colour: info, warning or danger.
	Kind string
}

var workerAppsTemplates = sync.OnceValue(func() *template.Template {
	return template.Must(template.ParseFS(templateFiles, "templates/worker_apps.html"))
})

// workerInstallsApps reports whether w can install apps through the Hub: it
// announced app.install.v1 and serves app_install.
func workerInstallsApps(w ops.WorkerInfo) bool {
	return proto.HasFeature(w.Features, proto.FeatureAppInstallV1) && slices.Contains(w.Ops, ops.OpAppInstall)
}

// appName is the name of the app of an outcome: its label, its package, or
// the file.
func appName(r ops.AppInstallResult) string {
	switch {
	case r.Label != "":
		return r.Label
	case r.Package != "":
		return r.Package
	case r.Path != "":
		return r.Path
	}
	return "the app"
}

// appOutcome puts an install outcome into words.
func appOutcome(r ops.AppInstallResult) AppOutcomeView {
	v := AppOutcomeView{AppInstallResult: r, Kind: "info"}
	name := appName(r)
	withReason := func(s string) string {
		if r.Reason != "" {
			return s + ": " + r.Reason
		}
		return s
	}
	switch r.Outcome {
	case ops.AppInstallInstalled:
		v.Text = name + " is installed."
	case ops.AppInstallNeedsConfirmation:
		v.Kind = "warning"
		v.Text = "Android asks for a confirmation on the device: press Install " + name + " on the Minion screen of the device."
	case ops.AppInstallInstalling:
		v.Text = name + " is being installed. Reload this panel for the outcome."
	case ops.AppInstallRefused:
		v.Kind = "danger"
		v.Text = withReason("Refused")
	case ops.AppInstallFailed:
		v.Kind = "danger"
		v.Text = withReason("Failed")
	default:
		v.Kind = "warning"
		v.Text = withReason("Unknown outcome " + r.Outcome)
	}
	return v
}

func workerAppsView(r *http.Request, t target) WorkerAppsView {
	v := WorkerAppsView{Show: t.operate && workerInstallsApps(t.worker), WorkerID: t.worker.ID, Worker: t.worker.Name, URL: WorkerAppsPath(t.worker.ID)}
	if v.Worker == "" {
		v.Worker = t.worker.ID
	}
	if !v.Show {
		return v
	}
	if !t.worker.Online {
		v.Error = "The worker is offline, so its Minion files folder cannot be read."
		return v
	}
	var st ops.AppInstallStatus
	if err := t.c.CallOp(r, ops.OpAppInstallStatus, ops.AppInstallStatusArgs{WorkerID: t.worker.ID}, &st); err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) && pe.Code == proto.CodeUnsupportedOp {
			v.Error = "This worker does not offer app installs (its version is older, or it is not the Android app)."
		} else {
			v.Error = failureText(err)
		}
		return v
	}
	v.Status = st
	if st.Last != nil {
		o := appOutcome(*st.Last)
		v.Last = &o
	}
	return v
}

func handleWorkerAppsGet(w http.ResponseWriter, r *http.Request, t target) {
	writeFragmentTemplate(w, workerAppsTemplates(), "worker_apps_panel", workerAppsView(r, t))
}

func handleWorkerAppsInstall(w http.ResponseWriter, r *http.Request, t target) {
	ar := ActionResult{Label: "Install", Kind: "info"}
	path := strings.TrimSpace(r.PostFormValue("path"))
	switch {
	case !t.operate:
		ar.Kind, ar.Summary = "danger", "Only the owner of this worker, or an administrator, can install apps on it."
	case !workerInstallsApps(t.worker):
		ar.Kind, ar.Summary = "danger", "This worker does not offer app installs."
	case ops.ValidAppInstallPath(path) != nil:
		ar.Kind, ar.Summary = "danger", "Choose an .apk file of the Minion files folder."
	default:
		args := ops.AppInstallArgs{WorkerID: t.worker.ID, Path: path, AllowNew: r.PostFormValue("allow_new") == "1"}
		var res ops.AppInstallResult
		if err := t.c.CallOp(r, ops.OpAppInstall, args, &res); err != nil {
			ar.Kind, ar.Summary = "danger", failureText(err)
		} else {
			if res.Path == "" {
				res.Path = path
			}
			o := appOutcome(res)
			ar.Kind, ar.Summary = o.Kind, o.Text
		}
	}
	v := workerAppsView(r, t)
	v.Results = []ActionResult{ar}
	writeFragmentTemplate(w, workerAppsTemplates(), "worker_apps_panel", v)
}

func init() {
	RegisterRoute(Route{Pattern: "GET " + pathWorkerAppsPattern, Handler: sessionRoute(handleWorkerAppsGet)})
	RegisterRoute(Route{Pattern: "POST " + pathWorkerAppsPattern, Handler: sessionRoute(handleWorkerAppsInstall)})
}
