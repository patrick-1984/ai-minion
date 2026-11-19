package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/version"
)

func withHubVersion(t *testing.T, v string) {
	t.Helper()
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

var wOld = WorkerRow{ID: "w1", Name: "laptop", Owner: "alice", Kind: ops.WorkerKindPrivate, Online: true, Version: "3.0", Build: "v0.8.2", SelfUpdate: true}

// An admin sees "v0.8.2 → v0.9.0" with Update and "Update all outdated"; a
// user sees the arrow only; a Worker that cannot update itself shows why.
func TestWorkersPageOffersTheUpdateToAdminsOnly(t *testing.T) {
	withHubVersion(t, "v0.9.0")
	blocked := wOld
	blocked.ID, blocked.Name, blocked.SelfUpdate, blocked.UpdateBlock = "w2", "nas", false, "installed by a package: upgrade it with apt or dnf"
	current := wOld
	current.ID, current.Name, current.Build = "w3", "fresh", "v0.9.0"
	admin := Dashboard{Admin: true, Workers: []WorkerRow{wOld, blocked, current}}
	user := Dashboard{Workers: []WorkerRow{wOld}}
	s, _ := listServer(t, user, admin)
	body := get(t, s, "/workers?scope=all", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `data-build="v0.8.2"`, `→ v0.9.0`, `action="/workers/w1/update"`, `Update all outdated (1)`,
		`(installed by a package: upgrade it with apt or dnf)`)
	mustNotHave(t, body, `action="/workers/w2/update"`, `action="/workers/w3/update"`)
	body = get(t, s, "/workers", "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `→ v0.9.0`)
	mustNotHave(t, body, `/update"`, `Update all outdated`)
}

// A development build of the Hub offers no update.
func TestDevHubOffersNoWorkerUpdate(t *testing.T) {
	withHubVersion(t, "dev")
	s, _ := listServer(t, Dashboard{Admin: true, Workers: []WorkerRow{wOld}}, Dashboard{Admin: true, Workers: []WorkerRow{wOld}})
	mustNotHave(t, get(t, s, "/workers", "X-Test-Session", adminHeader).Body.String(), `→`, `/update"`)
}

// The Update button sends worker_update with the Hub's exact release.
func TestWorkerUpdatePostsTheHubRelease(t *testing.T) {
	withHubVersion(t, "v0.9.0")
	f := newFakeOps().reply(ops.OpWorkerUpdate, ops.WorkerUpdateResult{Accepted: true, From: "v0.8.2", To: "v0.9.0"})
	connect(t, f)
	s := adminServer(t)
	rec := post(t, s, "/workers/w1/update", adminHeader, url.Values{})
	if rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	calls := f.called(ops.OpWorkerUpdate)
	if len(calls) != 1 || calls[0].Args["worker_id"] != "w1" || calls[0].Args["version"] != "v0.9.0" || calls[0].Args["force"] != nil {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestWorkerUpdateRefusalIsShown(t *testing.T) {
	withHubVersion(t, "v0.9.0")
	f := newFakeOps().reply(ops.OpWorkerUpdate, ops.WorkerUpdateResult{Reason: "refusing a downgrade from v0.9.1"})
	connect(t, f)
	rec := post(t, adminServer(t), "/workers/w1/update", adminHeader, url.Values{})
	mustHave(t, rec.Body.String(), "refusing a downgrade from v0.9.1")
}

// "Update all outdated" keeps the scope of the page it was on.
func TestUpdateAllKeepsThePageScope(t *testing.T) {
	withHubVersion(t, "v0.9.0")
	for _, tc := range []struct {
		form url.Values
		all  any
	}{{url.Values{}, nil}, {url.Values{"scope": {"all"}}, true}} {
		f := newFakeOps().reply(ops.OpWorkersList, ops.WorkersListResult{})
		connect(t, f)
		_ = post(t, adminServer(t), "/workers/update-all", adminHeader, tc.form)
		calls := f.called(ops.OpWorkersList)
		if len(calls) != 1 || calls[0].Args["all"] != tc.all {
			t.Errorf("form %v: workers_list args %+v", tc.form, calls)
		}
	}
}

// A54: in the Workers list, New session and Forget sit in one row; Forget asks
// for confirmation first.
func TestWorkersRowHasNewSessionAndForgetInline(t *testing.T) {
	withHubVersion(t, "v0.9.0")
	cur := wOld
	cur.Build = "v0.9.0"
	d := Dashboard{Admin: true, Workers: []WorkerRow{cur}}
	s, _ := listServer(t, d, d)
	body := get(t, s, "/workers?scope=all", "X-Test-Session", adminHeader).Body.String()
	i := strings.Index(body, `<div class="action-row">`)
	j := strings.Index(body, `data-control="forget"`)
	k := strings.Index(body, `data-control="new-session"`)
	if i < 0 || j < i || k < i {
		t.Fatalf("New session and Forget are not in one action row:\n%s", body)
	}
	mustHave(t, body, `hx-post="/workers/w1/remove"`, `hx-confirm="Forget worker laptop of alice (id w1)?`)
}
