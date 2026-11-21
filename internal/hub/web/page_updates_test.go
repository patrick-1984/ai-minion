package web

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The Updates page shows when the check ran and offers "Check now", which
// calls hub_update_refresh.
func TestUpdatesPageChecksNow(t *testing.T) {
	res := ops.HubUpdateCheckResult{Current: "v0.11.0", Channel: "latest",
		Latest: "v0.13.0", Newer: true, CheckedChannel: "latest", CheckedAt: "2026-11-18T19:05:00Z", Refreshed: true}
	f := newFakeOps().reply(ops.OpHubUpdateCheck, res).reply(ops.OpHubUpdateRefresh, res)
	connect(t, f)
	s := adminServer(t)
	body := get(t, s, pathAdminUpdates, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `hx-post="/admin/updates/check"`, `Check now`, `<time datetime="2026-11-18T19:05:00Z">2026-11-18 19:05 UTC</time>`, `v0.13.0`)
	rec := post(t, s, pathAdminUpdates+"/check", adminHeader, url.Values{})
	if rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	if calls := f.called(ops.OpHubUpdateRefresh); len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
}

// Right after update_channel changed, the page does not present the answer
// of the old channel as the newest release of the new one.
func TestUpdatesPageDoesNotShowTheOldChannelsAnswer(t *testing.T) {
	f := newFakeOps().reply(ops.OpHubUpdateCheck, ops.HubUpdateCheckResult{Current: "v0.11.0", Channel: "latest",
		Latest: "v0.12.0", Newer: true, ReleaseURL: "https://example.invalid/v0.12.0", SelfUpdate: true,
		CheckedChannel: "major", CheckedAt: "2026-11-18T19:05:00Z"})
	connect(t, f)
	s := adminServer(t)
	body := get(t, s, pathAdminUpdates, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `data-check-pending`, `checking the channel latest (the last answer was for major)`)
	// Neither the old answer nor an update to it is offered meanwhile.
	mustNotHave(t, body, `(up to date)`, `v0.12.0`, `data-control="hub-update"`, `cp-update-url`)
	// Check now still works meanwhile: its result target is on the page.
	mustHave(t, body, `hx-target="#hub-panel"`, `id="hub-panel"`, `id="list-result"`)
}

// While a check runs the panel polls itself; when it is done (or the bound is
// reached) the fragment carries no trigger, and Check now swaps the panel.
func TestUpdatesPanelPollsOnlyWhileChecking(t *testing.T) {
	running := ops.HubUpdateCheckResult{Current: "v0.11.0", Channel: "latest", Checking: true, Refreshed: true}
	f := newFakeOps().reply(ops.OpHubUpdateCheck, running).reply(ops.OpHubUpdateRefresh, running)
	connect(t, f)
	s := adminServer(t)
	page := get(t, s, pathAdminUpdates, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, page, `data-checking`, `hx-get="/admin/updates/panel?n=1"`, `hx-trigger="load delay:2s"`, `hx-target="#hub-panel"`)
	frag := get(t, s, pathUpdatesPanel+"?n=5", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, frag, `data-checking`, `hx-get="/admin/updates/panel?n=6"`)
	mustNotHave(t, frag, `<html`)
	// A hung check stops polling at the bound.
	mustNotHave(t, get(t, s, pathUpdatesPanel+"?n=90", "X-Test-Session", adminHeader).Body.String(), `hx-trigger`)
	// Check now answers with the polling panel.
	mustHave(t, post(t, s, pathAdminUpdates+"/check", adminHeader, url.Values{}).Body.String(), `id="hub-panel"`, `hx-trigger="load delay:2s"`)

	done := ops.HubUpdateCheckResult{Current: "v0.11.0", Channel: "latest", Latest: "v0.13.0", Newer: true,
		CheckedChannel: "latest", CheckedAt: "2026-11-18T19:05:00Z"}
	connect(t, newFakeOps().reply(ops.OpHubUpdateCheck, done).reply(ops.OpHubUpdateRefresh, done))
	frag = get(t, s, pathUpdatesPanel+"?n=3", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, frag, `v0.13.0`, `id="hub-panel"`)
	mustNotHave(t, frag, `hx-trigger="load`, `data-checking`)
}

func TestUpdatesPanelIsAdminOnly(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpHubUpdateCheck, ops.HubUpdateCheckResult{}))
	s := adminServer(t)
	if rec := get(t, s, pathUpdatesPanel, "X-Test-Session", userHeader); rec.Code != http.StatusNotFound {
		t.Fatalf("panel as user = %d, want 404", rec.Code)
	}
}
