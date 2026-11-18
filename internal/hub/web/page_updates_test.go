package web

import (
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
}
