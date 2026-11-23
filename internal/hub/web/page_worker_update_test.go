package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
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

// A failed attempt is shown next to the Update button on the Workers page and
// on the Worker page.
func TestUpdateFailureIsShownOnBothPages(t *testing.T) {
	withHubVersion(t, "v0.9.0")
	failed := wOld
	failed.UpdateFailed = "Update to v0.9.0 failed: v0.9.0 is not published on GitHub yet (2026-11-23 10:00 UTC)"
	d := Dashboard{Admin: true, Workers: []WorkerRow{failed}}
	s, _ := listServer(t, d, d)
	for _, path := range []string{"/workers", "/workers/w1"} {
		body := get(t, s, path, "X-Test-Session", adminHeader).Body.String()
		mustHave(t, body, `data-update-failed`, `Update to v0.9.0 failed: v0.9.0 is not published on GitHub yet`)
	}
	body := get(t, s, "/workers", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `action="/workers/w1/update"`)
}

// A release the Hub cannot see publicly gets a disabled button and the reason.
func TestUpdateIsDisabledForAnUnpublishedRelease(t *testing.T) {
	withHubVersion(t, "v0.9.0")
	SetPublishedSource(func(tag string) (bool, bool) { return false, true })
	t.Cleanup(func() { SetPublishedSource(nil) })
	d := Dashboard{Admin: true, Workers: []WorkerRow{wOld}}
	s, _ := listServer(t, d, d)
	body := get(t, s, "/workers", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `data-update-unpublished`, `v0.9.0 is not published yet`, `data-control="update" disabled`)
	mustNotHave(t, body, `action="/workers/w1/update"`)
	// An unknown state keeps the button.
	SetPublishedSource(func(string) (bool, bool) { return false, false })
	body = get(t, s, "/workers", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `action="/workers/w1/update"`)
	mustNotHave(t, body, `data-update-unpublished`)
}

func TestNotPublishedTextFollowsTheSource(t *testing.T) {
	t.Cleanup(func() { SetPublishedSource(nil) })
	if notPublishedText("v1.0.0") != "" {
		t.Error("no source must say nothing")
	}
	SetPublishedSource(func(tag string) (bool, bool) { return tag == "v1.0.0", true })
	if got := notPublishedText("v1.0.1"); got != "v1.0.1 is not published yet" {
		t.Errorf("unpublished = %q", got)
	}
	if got := notPublishedText("v1.0.0"); got != "" {
		t.Errorf("published = %q", got)
	}
}

// settle asks for the cached status until the background question has been
// answered (the call count reaches n) and returns the text then.
func settle(t *testing.T, r *http.Request, f *fakeOps, id, target string, n int) string {
	t.Helper()
	updateFailureText(r, f, id, target)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		updateFailMu.Lock()
		e := updateFailCache[id]
		updateFailMu.Unlock()
		if !e.inflight && len(f.called(ops.OpWorkerUpdateStatus)) >= n {
			return updateFailureText(r, f, id, target)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the update status was never answered")
	return ""
}

// The Hub reads the Worker's update status in the background: a failed
// attempt to the Hub's release shows with its real cause and time; an old
// Worker's "no matching release" gets the same wording as a new one; a
// rollback shows as one; other targets and states show nothing.
func TestUpdateFailureTextFromTheWorkerStatus(t *testing.T) {
	at := ops.Timestamp(time.Date(2026, 11, 23, 10, 5, 0, 0, time.UTC))
	status := ops.WorkerUpdateStatus{State: ops.UpdateStateFailed, To: "v0.9.0", Error: `no matching release for "v0.9.0"`, At: &at}
	f := newFakeOps().on(ops.OpWorkerUpdateStatus, func(map[string]any) (any, error) { return status, nil })
	r := httptest.NewRequest("GET", "/workers", nil)
	forgetUpdateFailure("wf")
	if first := updateFailureText(r, f, "wf", "v0.9.0"); first != "" {
		t.Fatalf("the first read must not wait for the Worker: %q", first)
	}
	got := settle(t, r, f, "wf", "v0.9.0", 1)
	want := "Update to v0.9.0 failed: v0.9.0 is not published on GitHub yet (the release is still a draft or missing); try again after it is published (2026-11-23 10:05 UTC)"
	if got != want {
		t.Fatalf("text = %q\nwant   %q", got, want)
	}
	// Reused within the TTL: no second call.
	updateFailureText(r, f, "wf", "v0.9.0")
	if n := len(f.called(ops.OpWorkerUpdateStatus)); n != 1 {
		t.Errorf("status calls = %d, want 1 (cached)", n)
	}
	// A rollback names the failed target (its From) and says so.
	status = ops.WorkerUpdateStatus{State: ops.UpdateStateRolledBack, From: "v0.9.0", Running: "v0.8.9", Error: "the new binary exited at start", At: &at}
	forgetUpdateFailure("wf")
	got = settle(t, r, f, "wf", "v0.9.0", 2)
	if !strings.Contains(got, "Update to v0.9.0 was rolled back to the previous version: the new binary exited at start") || !strings.Contains(got, "2026-11-23 10:05 UTC") {
		t.Errorf("rolled back = %q", got)
	}
	// A failure to another version, or a state that is not failed: nothing.
	status = ops.WorkerUpdateStatus{State: ops.UpdateStateFailed, To: "v0.8.9"}
	forgetUpdateFailure("wf")
	if got := settle(t, r, f, "wf", "v0.9.0", 3); got != "" {
		t.Errorf("other target = %q", got)
	}
	status = ops.WorkerUpdateStatus{State: ops.UpdateStateIdle}
	forgetUpdateFailure("wf")
	if got := settle(t, r, f, "wf", "v0.9.0", 4); got != "" {
		t.Errorf("idle = %q", got)
	}
}

// A Worker that never answers delays no page: the read returns at once and
// asks only once while the question is open.
func TestUpdateStatusOfASilentWorkerNeverBlocksAPage(t *testing.T) {
	release := make(chan struct{})
	f := newFakeOps().on(ops.OpWorkerUpdateStatus, func(map[string]any) (any, error) { <-release; return ops.WorkerUpdateStatus{}, nil })
	t.Cleanup(func() { close(release) })
	r := httptest.NewRequest("GET", "/workers", nil)
	forgetUpdateFailure("silent")
	start := time.Now()
	for range 5 {
		updateFailureText(r, f, "silent", "v0.9.0")
	}
	if time.Since(start) > time.Second {
		t.Fatal("reading the update status waited for the Worker")
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(f.called(ops.OpWorkerUpdateStatus)); n > 1 {
		t.Errorf("%d questions in flight for one Worker", n)
	}
}

// A release the Hub knows to be unpublished is never sent: not by the
// per-Worker form (also a stale one) and not by Update all outdated.
func TestUpdateNeverDispatchesAKnownUnpublishedRelease(t *testing.T) {
	withHubVersion(t, "v0.9.0")
	SetPublishedSource(func(string) (bool, bool) { return false, true })
	t.Cleanup(func() { SetPublishedSource(nil) })
	f := newFakeOps().reply(ops.OpWorkerUpdate, ops.WorkerUpdateResult{Accepted: true}).
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{{ID: "w1", Name: "laptop", Online: true, Build: "v0.8.0", Features: []string{proto.FeatureSelfUpdate}}}})
	connect(t, f)
	s := adminServer(t)
	one := post(t, s, "/workers/w1/update", adminHeader, url.Values{})
	all := post(t, s, WorkersUpdateAllPath, adminHeader, url.Values{})
	if n := len(f.called(ops.OpWorkerUpdate)); n != 0 {
		t.Fatalf("%d updates were sent for an unpublished release", n)
	}
	mustHave(t, one.Body.String(), "v0.9.0 is not published yet")
	mustHave(t, all.Body.String(), "v0.9.0 is not published yet")
}

// A refusal that carries the old wording is reworded in the answer.
func TestWorkerUpdateRefusalWordsAnUnpublishedRelease(t *testing.T) {
	withHubVersion(t, "v0.9.0")
	f := newFakeOps().reply(ops.OpWorkerUpdate, ops.WorkerUpdateResult{Reason: `no matching release for "v0.9.0"`})
	connect(t, f)
	rec := post(t, adminServer(t), "/workers/w1/update", adminHeader, url.Values{})
	mustHave(t, rec.Body.String(), "v0.9.0 is not published on GitHub yet")
}
