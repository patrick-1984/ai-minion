//go:build e2e && unix

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// v0.25.0: a session on one Worker installs apps on another (Android) Worker
// only through the master lane, decided on the apps_install flag of a
// verified grant. The Worker side of the lane is played by the test on
// Worker A's real Hub link as in files_master_unix_test.go; Worker B has the
// app install end of the Android Worker without the app, so it answers
// app_install_status (not ready, the APK files of its Minion files folder)
// and refuses app_install.
func TestAppInstallSessionCallerVerifiedSource(t *testing.T) {
	e := startFilesE2E(t)
	a := e.startWorker("source-box", fxWorkerOptions{master: true}, "boss")
	b := e.startWorker("phone", fxWorkerOptions{apps: true}, "build")
	writeFile(t, b.dirs["build"], "example.apk", []byte("not really an apk"))
	m := e.startMasterLane(a, "boss")
	waitFor(t, "app_install_v1 in the welcome", func() bool { return a.link.Load().Welcome.AppInstallV1 })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	status := ops.AppInstallStatusArgs{WorkerID: b.id}
	install := ops.AppInstallArgs{WorkerID: b.id, Path: "example.apk", AllowNew: true}

	// A claimed source never installs: the call stream is refused with what
	// to do.
	err := claimedCall(ctx, a, "boss", ops.OpAppInstall, install)
	if codeOf(err) != proto.CodeForbidden || !strings.Contains(err.Error(), "verified source") || !strings.Contains(err.Error(), "apps_install") {
		t.Fatalf("claimed install = %v", err)
	}
	if err := claimedCall(ctx, a, "boss", ops.OpAppInstallStatus, status); codeOf(err) != proto.CodeForbidden {
		t.Fatalf("claimed status = %v", err)
	}

	// A bound grant without apps_install: a verified lease, no authority.
	g := e.grantSet(a, b, "", 0, nil, nil)
	bind := true
	code, body := e.ph.do("POST", "/api/v1/ops/"+ops.OpGrantBind, ops.GrantBindArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()},
		ID: g.ID, IfRevision: g.Revision, Bind: &bind}, e.owner)
	var bound ops.GrantSetResult
	if code != http.StatusOK || json.Unmarshal([]byte(body), &bound) != nil {
		t.Fatalf("grant_bind: %d %s", code, body)
	}
	waitFor(t, "the lease of the bound grant", func() bool { return m.lease().LeaseID != "" })
	if err := m.call(ctx, ops.OpAppInstall, install, nil); codeOf(err) != proto.CodeForbidden {
		t.Fatalf("install without apps_install = %v", err)
	}
	if err := m.call(ctx, ops.OpAppInstallStatus, status, nil); codeOf(err) != proto.CodeForbidden {
		t.Fatalf("status without apps_install = %v", err)
	}

	// apps_install on the bound grant: the calls reach the Android Worker.
	yes := true
	args := ops.GrantSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, ID: g.ID, IfRevision: bound.Grant.Revision,
		SourceWorkerID: a.id, SourceSession: "boss", TargetWorkerID: b.id, TargetSession: "build", Level: ops.GrantLevelControl,
		AppsInstall: &yes}
	if code, body := e.ph.do("POST", "/api/v1/ops/"+ops.OpGrantSet, args, e.owner); code != http.StatusOK {
		t.Fatalf("grant_set apps_install: %d %s", code, body)
	}
	var st ops.AppInstallStatus
	if err := m.call(ctx, ops.OpAppInstallStatus, status, &st); err != nil {
		t.Fatalf("status with apps_install: %v", err)
	}
	if st.Ready || len(st.Files) != 1 || st.Files[0].Path != "example.apk" {
		t.Fatalf("status = %+v", st)
	}
	var res ops.AppInstallResult
	if err := m.call(ctx, ops.OpAppInstall, install, &res); err != nil {
		t.Fatalf("install with apps_install: %v", err)
	}
	if res.Outcome != ops.AppInstallRefused || res.Path != "example.apk" {
		t.Fatalf("install = %+v", res)
	}

	// The Hub audited the install of the session with its outcome and the
	// grant whose apps_install flag decided it (the denied attempt before
	// apps_install too, which no flag decided).
	code, body = e.ph.do("POST", "/api/v1/ops/"+ops.OpAuditList, ops.AuditListArgs{Action: ops.OpAppInstall}, e.ph.admin)
	var list ops.AuditListResult
	if code != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil {
		t.Fatalf("audit_list: %d %s", code, body)
	}
	refused, denied := false, false
	for _, ev := range list.Events {
		if ev.Source != a.id+":boss" || ev.Target != b.id || !strings.Contains(ev.Detail, "proof=bound") {
			continue
		}
		refused = refused || ev.Outcome == ops.AppInstallRefused && strings.Contains(ev.Detail, "reason=") && ev.GrantID == g.ID
		denied = denied || ev.Outcome == "denied" && ev.GrantID == ""
	}
	if !refused || !denied {
		t.Fatalf("no audit row of the session's install: %s", body)
	}
}
