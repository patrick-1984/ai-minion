package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The Device control panel: an Android Worker shows its status, the last audit
// entries newest first and a Disable button; Disable calls adb_disable for the
// owner only; a Worker that is not Android shows nothing; there is no enable.
func TestWorkerDevicePanel(t *testing.T) {
	d := Dashboard{Workers: []WorkerRow{wPrivate}}
	w := wAliceInfo
	w.OS = "android"
	bob := wBobInfo
	bob.OS, bob.Online = "android", true
	on := ops.DeviceControlStatus{Available: true, Enabled: true, State: "READY", Port: 37000, Transport: "tls-loopback", KeyStorage: "app-file", ActiveStreams: 1}
	f := newFakeOps().
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{w, bob}}).
		reply(ops.OpAdbStatus, ops.AdbStatusResult{DeviceControl: on, Audit: []ops.AdbAuditEntry{
			{Time: "2026-11-27T10:00:00Z", Actor: "local", Session: "build", Op: "shell", Transport: "tls-loopback", Result: "exit:7", DurationMS: 12},
			{Time: "2026-11-27T10:01:00Z", Actor: "local", Session: "docs", Op: "screencap", Transport: "tls-loopback", Result: "ok", DurationMS: 30},
		}}).
		reply(ops.OpAdbDisable, ops.AdbDisableResult{DeviceControl: ops.DeviceControlStatus{Available: true, State: "OFF"}})
	connect(t, f)
	s, _ := listServer(t, d, d)

	body := get(t, s, "/w/w1/device-control", "X-Test-Session", "alice:user").Body.String()
	mustHave(t, body, `id="device-section"`, "READY", "TLS on loopback, port 37000", `data-control="device-disable"`, `hx-post="/w/w1/device-control"`,
		"Every terminal of this Worker could read it.", "exit:7", "needs a dedicated, unshared worker")
	if strings.Index(body, "screencap") > strings.Index(body, "exit:7") {
		t.Fatal("the audit is not newest first")
	}
	if strings.Contains(strings.ToLower(body), "enable device control") {
		t.Fatal("the Hub offers to enable device control")
	}
	if len(f.called(ops.OpAdbDisable)) != 0 {
		t.Fatal("a GET disabled device control")
	}

	rec := post(t, s, "/w/w1/device-control", "alice:user", url.Values{})
	mustHave(t, rec.Body.String(), "Device control is disabled", "Forget")
	if calls := f.called(ops.OpAdbDisable); len(calls) != 1 || calls[0].Args["worker_id"] != w.ID {
		t.Fatalf("calls = %+v", calls)
	}

	// Bob's Worker: alice may not disable it; nothing is called.
	rec = post(t, s, "/w/w2/device-control", "alice:user", url.Values{})
	if len(f.called(ops.OpAdbDisable)) != 1 {
		t.Fatalf("adb_disable was called for a person who may not operate the worker: %s", rec.Body.String())
	}

	// A Worker that is not Android: the panel renders nothing.
	w.OS = "linux"
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{w, bob}})
	if body := get(t, s, "/w/w1/device-control", "X-Test-Session", "alice:user").Body.String(); strings.TrimSpace(body) != "" {
		t.Fatalf("a linux worker shows a device control panel: %q", body)
	}
}
