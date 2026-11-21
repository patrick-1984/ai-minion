package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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
	mustHave(t, body, `hx-target="#hub-release-facts"`, `id="hub-release-facts"`, `id="list-result"`)
}

func panelGet(t *testing.T, s http.Handler, q string) *httptest.ResponseRecorder {
	t.Helper()
	return get(t, s, pathUpdatesPanel+q, "X-Test-Session", adminHeader)
}

// While a check runs the release facts poll themselves; when it is done (or
// the bound is reached) the fragment carries no trigger.
func TestUpdatesPanelPollsOnlyWhileChecking(t *testing.T) {
	running := ops.HubUpdateCheckResult{Current: "v0.11.0", Channel: "latest", Checking: true, Refreshed: true}
	connect(t, newFakeOps().reply(ops.OpHubUpdateCheck, running).reply(ops.OpHubUpdateRefresh, running))
	s := adminServer(t)
	page := get(t, s, pathAdminUpdates, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, page, `data-checking`, `hx-get="/admin/updates/panel?n=1&amp;since=0"`, `hx-trigger="load delay:2s"`, `id="hub-live" aria-live="polite"`)
	// The update form and its result are outside the swapped element.
	if strings.Index(page, `id="list-result"`) < strings.Index(page, `id="hub-release-facts"`) {
		t.Error("list-result must come after the facts")
	}
	frag := panelGet(t, s, "?n=5").Body.String()
	mustHave(t, frag, `data-checking`, `n=6`)
	mustNotHave(t, frag, `<html`, `data-control=`)
	mustNotHave(t, panelGet(t, s, "?n=90").Body.String(), `hx-trigger`)
	mustHave(t, post(t, s, pathAdminUpdates+"/check", adminHeader, url.Values{}).Body.String(), `id="hub-release-facts"`, `hx-trigger="load delay:2s"`, `since=`)

	done := ops.HubUpdateCheckResult{Current: "v0.11.0", Channel: "latest", Latest: "v0.13.0", Newer: true, ReleaseURL: "https://example.invalid/v0.13.0",
		CheckedChannel: "latest", CheckedAt: "2026-11-18T19:05:00Z"}
	connect(t, newFakeOps().reply(ops.OpHubUpdateCheck, done))
	rec := panelGet(t, s, "?n=3")
	mustHave(t, rec.Body.String(), `v0.13.0`, `id="hub-release-facts"`, `id="hub-release-link"`)
	mustNotHave(t, rec.Body.String(), `hx-trigger="load`, `data-checking`)
	if rec.Header().Get("HX-Refresh") != "true" {
		t.Error("a finished check reloads the page once so that the buttons are current")
	}
}

// A check that finished while the Hub probed Docker (Checking already false,
// but the answer is older than the press) keeps polling until a newer answer.
func TestUpdatesPanelKeepsPollingUntilTheAnswerIsNewerThanThePress(t *testing.T) {
	old := ops.HubUpdateCheckResult{Current: "v0.11.0", Channel: "latest", Latest: "v0.12.0",
		CheckedChannel: "latest", CheckedAt: "2026-11-18T19:05:00Z"}
	connect(t, newFakeOps().reply(ops.OpHubUpdateCheck, old))
	s := adminServer(t)
	since := fmt.Sprint(time.Date(2026, 11, 18, 19, 10, 0, 0, time.UTC).Unix())
	rec := panelGet(t, s, "?n=1&since="+since)
	mustHave(t, rec.Body.String(), `hx-trigger="load delay:2s"`, `since=`+since)
	if rec.Header().Get("HX-Refresh") != "" {
		t.Error("no reload while the answer is older than the press")
	}
	connect(t, newFakeOps().reply(ops.OpHubUpdateCheck, ops.HubUpdateCheckResult{Current: "v0.11.0", Channel: "latest",
		Latest: "v0.13.0", CheckedChannel: "latest", CheckedAt: "2026-11-18T19:10:03Z"}))
	rec = panelGet(t, s, "?n=2&since="+since)
	mustNotHave(t, rec.Body.String(), `hx-trigger`)
	mustHave(t, rec.Body.String(), `v0.13.0`)
	if rec.Header().Get("HX-Refresh") != "true" {
		t.Error("done: reload once")
	}
}

// A failed poll or a failed Check now keeps the facts element (an alert inside
// it), never an empty or partial body that would swap the panel away.
func TestUpdatesPanelErrorsKeepTheElement(t *testing.T) {
	boom := func(map[string]any) (any, error) { return nil, errors.New("boom") }
	connect(t, newFakeOps().on(ops.OpHubUpdateCheck, boom).on(ops.OpHubUpdateRefresh, boom))
	s := adminServer(t)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"poll":  panelGet(t, s, "?n=2"),
		"check": post(t, s, pathAdminUpdates+"/check", adminHeader, url.Values{}),
	} {
		if rec.Code != 200 {
			t.Errorf("%s = %d", name, rec.Code)
		}
		mustHave(t, rec.Body.String(), `id="hub-release-facts"`, `role="alert"`)
		mustNotHave(t, rec.Body.String(), `hx-trigger`)
		if rec.Header().Get("HX-Refresh") != "" {
			t.Errorf("%s: an error must not reload", name)
		}
	}
}

func TestUpdatesPanelIsAdminOnly(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpHubUpdateCheck, ops.HubUpdateCheckResult{}))
	s := adminServer(t)
	if rec := get(t, s, pathUpdatesPanel, "X-Test-Session", userHeader); rec.Code != http.StatusNotFound {
		t.Fatalf("panel as user = %d, want 404", rec.Code)
	}
}
