package web

// Administration > Updates (v0.9.0, A10): the Hub's release, the newest one
// in its update channel, the Hub's own update (Docker, opt-in) and the
// Workers that are older than the Hub.

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

const pathAdminUpdates = "/admin/updates"

// UpdatesPage is what updates.html sees as .Data.
type UpdatesPage struct {
	Check ops.HubUpdateCheckResult
	// CheckedAt is Check.CheckedAt for people ("2026-11-18 19:05 UTC").
	CheckedAt string
	// Refresh is true while a check still runs (it took longer than the
	// wait of Check now): the page then reloads itself every 5 seconds.
	Refresh bool
	// Slow is true when the check has run for more than updateWatchMax: the
	// page stops reloading and says so.
	Slow bool
}

const (
	pathUpdatesCheck = pathAdminUpdates + "/check"
	// updateWait bounds how long Check now waits for the answer.
	updateWait = 20 * time.Second
	// updateWatchMax bounds how long the page reloads itself for one check.
	updateWatchMax = 2 * time.Minute
)

// checkWatch remembers since when the page has seen a check run, so that a
// hung check does not reload the page forever.
var checkWatch struct {
	mu    sync.Mutex
	since time.Time
}

// watchChecking reports how long the check has been seen running (zero when
// none runs).
func watchChecking(checking bool, now time.Time) time.Duration {
	checkWatch.mu.Lock()
	defer checkWatch.mu.Unlock()
	if !checking {
		checkWatch.since = time.Time{}
		return 0
	}
	if checkWatch.since.IsZero() {
		checkWatch.since = now
	}
	return now.Sub(checkWatch.since)
}

func updatesData(r *http.Request, c OpCaller) (any, error) {
	var res UpdatesPage
	if err := c.CallOp(r, ops.OpHubUpdateCheck, ops.HubUpdateCheckArgs{}, &res.Check); err != nil {
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339, res.Check.CheckedAt); err == nil {
		res.CheckedAt = t.UTC().Format("2006-01-02 15:04 UTC")
	}
	if res.Check.Checking {
		res.Slow = watchChecking(true, time.Now()) > updateWatchMax
		res.Refresh = !res.Slow
	} else {
		watchChecking(false, time.Now())
	}
	return res, nil
}

// handleUpdateCheckNow asks GitHub now ("Check now"), waits for the answer
// (at most updateWait) and sends the browser back to the page, which renders
// from fresh state. A page still checking after the wait reloads itself.
func handleUpdateCheckNow(w http.ResponseWriter, r *http.Request, c OpCaller) {
	var res ops.HubUpdateCheckResult
	if err := c.CallOp(r, ops.OpHubUpdateRefresh, ops.HubUpdateRefreshArgs{}, &res); err != nil {
		writeResultError(w, failureText(err))
		return
	}
	if !res.Refreshed {
		AddFlash(w, r, FlashInfo, "Not checked again: the last check was less than a minute ago, or the release check is off (update_check). The page shows the last answer.")
	} else {
		watchChecking(res.Checking, time.Now())
		deadline := time.Now().Add(updateWait)
		for res.Checking && time.Now().Before(deadline) && r.Context().Err() == nil {
			time.Sleep(200 * time.Millisecond)
			res = ops.HubUpdateCheckResult{} // omitted false fields must not keep an old true
			if err := c.CallOp(r, ops.OpHubUpdateCheck, ops.HubUpdateCheckArgs{}, &res); err != nil {
				break
			}
		}
	}
	// htmx carries the CSRF header, so it posts; it follows HX-Redirect with
	// a plain page load. Anything else gets the 303.
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", pathAdminUpdates)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, pathAdminUpdates, http.StatusSeeOther)
}

func handleHubUpdate(w http.ResponseWriter, r *http.Request, c OpCaller) {
	var res ops.HubUpdateResult
	err := c.CallOp(r, ops.OpHubUpdate, ops.HubUpdateArgs{Force: r.PostFormValue("force") == "1"}, &res)
	switch {
	case err != nil:
		writeResultError(w, failureText(err))
	case !res.Started:
		writeResultError(w, "The Hub was not updated: "+res.Reason+".")
	default:
		done(w, r, fmt.Sprintf("Updating the Hub from %s to %s. It restarts within a minute; reload this page then. If the new release does not become ready, the previous one starts again.", res.From, res.To), ops.WriteResult{})
	}
}

func init() {
	RegisterPage(Page{Pattern: pathAdminUpdates, Title: "Updates", Template: "updates.html", Data: adminPageData(updatesData)})
	RegisterNav(NavItem{Label: "Updates", Path: pathAdminUpdates, Order: adminNavOrder + 12, Roles: []authz.Role{authz.RoleAdmin}, Group: NavGroupAdmin, Icon: "i-settings"})
	RegisterRoute(Route{Pattern: "POST " + pathAdminUpdates + "/hub", Handler: adminRoute(handleHubUpdate)})
	RegisterRoute(Route{Pattern: "POST " + pathUpdatesCheck, Handler: adminRoute(handleUpdateCheckNow)})
}
