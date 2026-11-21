package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
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
	mustHave(t, body, `hx-post="/admin/updates/check"`, `id="hub-release-facts"`, `id="list-result"`)
}

// Check now waits for the check, then redirects to the page (HX-Redirect for
// htmx, which carries the CSRF header; 303 otherwise).
func TestUpdatesCheckNowWaitsAndRedirects(t *testing.T) {
	var polls atomic.Int32
	f := newFakeOps().
		reply(ops.OpHubUpdateRefresh, ops.HubUpdateCheckResult{Current: "v0.11.0", Channel: "latest", Checking: true, Refreshed: true}).
		on(ops.OpHubUpdateCheck, func(map[string]any) (any, error) {
			return ops.HubUpdateCheckResult{Current: "v0.11.0", Channel: "latest", Checking: polls.Add(1) < 2, Refreshed: true}, nil
		})
	connect(t, f)
	s := adminServer(t)
	rec := post(t, s, pathUpdatesCheck, adminHeader, url.Values{})
	if rec.Code != 200 || rec.Header().Get("HX-Redirect") != pathAdminUpdates {
		t.Fatalf("htmx = %d %v", rec.Code, rec.Header())
	}
	if n := len(f.called(ops.OpHubUpdateCheck)); n < 2 {
		t.Fatalf("waited for %d polls, want at least 2", n)
	}
	req := httptest.NewRequest(http.MethodPost, pathUpdatesCheck, strings.NewReader(""))
	req.Header.Set("X-Test-Session", adminHeader)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != pathAdminUpdates {
		t.Fatalf("plain = %d %v", rr.Code, rr.Header())
	}
}

func TestUpdatesCheckNowCooldownNotice(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpHubUpdateRefresh, ops.HubUpdateCheckResult{Current: "v0.11.0", Channel: "latest"}))
	rec := post(t, adminServer(t), pathUpdatesCheck, adminHeader, url.Values{})
	fl := flashesOf(rec)
	if len(fl) != 1 || !strings.Contains(fl[0].Text, "Not checked again") || rec.Header().Get("HX-Redirect") != pathAdminUpdates {
		t.Fatalf("flashes %+v, headers %v", fl, rec.Header())
	}
}

func TestUpdatesCheckNowFailureStaysOnThePage(t *testing.T) {
	connect(t, newFakeOps().on(ops.OpHubUpdateRefresh, func(map[string]any) (any, error) { return nil, errors.New("boom") }))
	rec := post(t, adminServer(t), pathUpdatesCheck, adminHeader, url.Values{})
	mustHave(t, rec.Body.String(), `role="alert"`)
	if rec.Header().Get("HX-Redirect") != "" {
		t.Error("an error is shown in place")
	}
}

// Only a running check adds the meta refresh, for at most updateWatchMax.
func TestUpdatesPageRefreshesOnlyWhileChecking(t *testing.T) {
	running := ops.HubUpdateCheckResult{Current: "v0.11.0", Channel: "latest", Checking: true, Newer: true, Latest: "v0.12.0",
		SelfUpdate: true, CheckingSince: time.Now().UTC().Format(time.RFC3339)}
	connect(t, newFakeOps().reply(ops.OpHubUpdateCheck, running))
	s := adminServer(t)
	body := get(t, s, pathAdminUpdates, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `<meta http-equiv="refresh" content="5">`, `data-checking`, `id="list-result"`)
	mustNotHave(t, body, `data-check-slow`)
	// An upgrade form is not offered while the page reloads itself.
	mustHave(t, body, `data-update-after-check`)
	mustNotHave(t, body, `data-control="hub-update"`)

	running.CheckingSince = time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339)
	connect(t, newFakeOps().reply(ops.OpHubUpdateCheck, running))
	body = get(t, s, pathAdminUpdates, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `data-check-slow`, `press Check now again later`)
	mustNotHave(t, body, `http-equiv="refresh"`)

	connect(t, newFakeOps().reply(ops.OpHubUpdateCheck, ops.HubUpdateCheckResult{Current: "v0.11.0", Channel: "latest",
		Latest: "v0.13.0", Newer: true, ReleaseURL: "https://example.invalid/v0.13.0", CheckedChannel: "latest", CheckedAt: "2026-11-18T19:05:00Z"}))
	body = get(t, s, pathAdminUpdates, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `v0.13.0`, `id="hub-release-link"`)
	mustNotHave(t, body, `http-equiv="refresh"`, `data-checking`, `data-check-slow`)
}

func TestUpdatesCheckIsAdminOnlyAndThePanelRouteIsGone(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpHubUpdateCheck, ops.HubUpdateCheckResult{}).reply(ops.OpHubUpdateRefresh, ops.HubUpdateCheckResult{}))
	s := adminServer(t)
	if rec := post(t, s, pathUpdatesCheck, userHeader, url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("check as user = %d, want 404", rec.Code)
	}
	if rec := get(t, s, pathAdminUpdates+"/panel", "X-Test-Session", adminHeader); rec.Code == 200 {
		t.Fatalf("the panel route still exists")
	}
}

// The whole Check now request is bounded: a check that never ends redirects
// after updateWait (the page then shows Checking and reloads itself).
func TestUpdatesCheckNowIsBounded(t *testing.T) {
	old := updateWait
	updateWait = 600 * time.Millisecond
	defer func() { updateWait = old }()
	forever := ops.HubUpdateCheckResult{Current: "v0.11.0", Channel: "latest", Checking: true, Refreshed: true}
	connect(t, newFakeOps().reply(ops.OpHubUpdateRefresh, forever).reply(ops.OpHubUpdateCheck, forever))
	start := time.Now()
	rec := post(t, adminServer(t), pathUpdatesCheck, adminHeader, url.Values{})
	if d := time.Since(start); d > 3*time.Second || rec.Header().Get("HX-Redirect") != pathAdminUpdates {
		t.Fatalf("took %v, headers %v", d, rec.Header())
	}
}
