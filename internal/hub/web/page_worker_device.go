package web

// The Device control panel of the Worker detail page: the on-device adb
// status of an Android Worker (adb_status, routed to the Worker), its last 20
// audit entries, and a Disable button (adb_disable). The Hub can disable,
// never enable: enabling is a choice made on the device. The panel renders
// nothing for a Worker that is not Android.

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sync"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

const pathWorkerDevicePattern = "/w/{worker}/device-control"

// WorkerDevicePath is the Device control panel of a Worker: GET renders it,
// POST (CSRF-checked) disables device control.
func WorkerDevicePath(worker string) string {
	return "/w/" + url.PathEscape(worker) + "/device-control"
}

// WorkerDeviceView is what the "worker_device_panel" fragment sees.
type WorkerDeviceView struct {
	// Show is false for a Worker that is not Android: the panel is left out.
	Show     bool
	WorkerID string
	Worker   string
	URL      string
	Status   ops.DeviceControlStatus
	// Audit is newest first.
	Audit   []ops.AdbAuditEntry
	Error   string
	Results []ActionResult
}

var workerDeviceTemplates = sync.OnceValue(func() *template.Template {
	return template.Must(template.ParseFS(templateFiles, "templates/worker_device.html"))
})

func workerDeviceView(r *http.Request, t target) WorkerDeviceView {
	v := WorkerDeviceView{Show: t.worker.OS == "android" && t.operate, WorkerID: t.worker.ID, Worker: t.worker.Name, URL: WorkerDevicePath(t.worker.ID)}
	if v.Worker == "" {
		v.Worker = t.worker.ID
	}
	if !v.Show {
		return v
	}
	if !t.worker.Online {
		v.Error = "The worker is offline, so its device control status is not known."
		return v
	}
	var res ops.AdbStatusResult
	if err := t.c.CallOp(r, ops.OpAdbStatus, ops.AdbStatusArgs{WorkerID: t.worker.ID}, &res); err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) && pe.Code == proto.CodeUnsupportedOp {
			v.Error = "This worker does not offer device control (its version is older, or it is not the Android app)."
		} else {
			v.Error = failureText(err)
		}
		return v
	}
	v.Status = res.DeviceControl
	v.Audit = slices.Clone(res.Audit)
	slices.Reverse(v.Audit)
	return v
}

func handleWorkerDeviceGet(w http.ResponseWriter, r *http.Request, t target) {
	writeFragmentTemplate(w, workerDeviceTemplates(), "worker_device_panel", workerDeviceView(r, t))
}

func handleWorkerDeviceDisable(w http.ResponseWriter, r *http.Request, t target) {
	ar := ActionResult{Label: "Device control", Kind: "info"}
	if !t.operate {
		ar.Kind, ar.Summary = "danger", "Only the owner of this worker, or an administrator, can disable device control."
	} else {
		var res ops.AdbDisableResult
		if err := t.c.CallOp(r, ops.OpAdbDisable, ops.AdbDisableArgs{WorkerID: t.worker.ID}, &res); err != nil {
			ar.Kind, ar.Summary = "danger", failureText(err)
		} else {
			ar.Summary = "Device control is disabled and its open streams are closed. It can be enabled again only on the device. To end the authorization in Android too, open Wireless debugging > Paired devices > minion@cc.nasz.minion > Forget on the device."
		}
	}
	v := workerDeviceView(r, t)
	v.Results = []ActionResult{ar}
	writeFragmentTemplate(w, workerDeviceTemplates(), "worker_device_panel", v)
}

func init() {
	RegisterRoute(Route{Pattern: "GET " + pathWorkerDevicePattern, Handler: sessionRoute(handleWorkerDeviceGet)})
	RegisterRoute(Route{Pattern: "POST " + pathWorkerDevicePattern, Handler: sessionRoute(handleWorkerDeviceDisable)})
}
