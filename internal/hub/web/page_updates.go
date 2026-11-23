package web

// Administration > Updates (v0.9.0, A10): the Hub's release, the newest one
// in its update channel, the Hub's own update (Docker, opt-in) and the
// Workers that are older than the Hub.

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
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
	// DocsBase is the repository docs of the running version (its release tag;
	// main for a build that is not a plain release): the guides the panel
	// "Automatic Hub updates" links to.
	DocsBase string
}

var releaseTag = regexp.MustCompile(`^v?(\d+\.\d+\.\d+)$`)

// docsBase is where the install guides of this Hub's version live on GitHub.
func docsBase(current string) string {
	ref := "main"
	if m := releaseTag.FindStringSubmatch(current); m != nil {
		ref = "v" + m[1]
	}
	return "https://github.com/patrick-1984/ai-minion/blob/" + ref + "/docs/install/"
}

// updateWait bounds the whole Check now request (under the Hub's 15 s shutdown budget).
var updateWait = 12 * time.Second

const (
	pathUpdatesCheck = pathAdminUpdates + "/check"
	// updateWatchMax bounds how long the page reloads itself for one check.
	updateWatchMax = 2 * time.Minute
)

func updatesData(r *http.Request, c OpCaller) (any, error) {
	var res UpdatesPage
	if err := c.CallOp(r, ops.OpHubUpdateCheck, ops.HubUpdateCheckArgs{}, &res.Check); err != nil {
		return nil, err
	}
	res.DocsBase = docsBase(res.Check.Current)
	if t, err := time.Parse(time.RFC3339, res.Check.CheckedAt); err == nil {
		res.CheckedAt = t.UTC().Format("2006-01-02 15:04 UTC")
	}
	if res.Check.Checking {
		since, err := time.Parse(time.RFC3339, res.Check.CheckingSince)
		res.Slow = err == nil && time.Since(since) > updateWatchMax
		res.Refresh = !res.Slow
	}
	return res, nil
}

// handleUpdateCheckNow asks GitHub now ("Check now"), waits for the answer
// (at most updateWait) and sends the browser back to the page, which renders
// from fresh state. A page still checking after the wait reloads itself.
func handleUpdateCheckNow(w http.ResponseWriter, r *http.Request, c OpCaller) {
	ctx, cancel := context.WithTimeout(r.Context(), updateWait)
	defer cancel()
	r = r.WithContext(ctx)
	var res ops.HubUpdateCheckResult
	if err := c.CallOp(r, ops.OpHubUpdateRefresh, ops.HubUpdateRefreshArgs{}, &res); err != nil {
		writeResultError(w, failureText(err))
		return
	}
	if !res.Refreshed {
		AddFlash(w, r, FlashInfo, "Not checked again: the last check was less than a minute ago, or the release check is off (update_check). The page shows the last answer.")
	} else {
		for res.Checking {
			select {
			case <-ctx.Done():
			case <-time.After(200 * time.Millisecond):
			}
			if ctx.Err() != nil {
				break
			}
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
	case res.Queued:
		done(w, r, fmt.Sprintf("Update to %s queued for the host updater's next Cron run. No restart has happened yet; the request expires in one hour.", res.To), ops.WriteResult{})
	case !res.Started:
		writeResultError(w, "The Hub was not updated: "+res.Reason+".")
	default:
		done(w, r, fmt.Sprintf("Updating the Hub from %s to %s. It restarts within a minute; reload this page then. If the new release does not become ready, the previous one starts again.", res.From, res.To), ops.WriteResult{})
	}
}

func handleHubUpdatePolicy(w http.ResponseWriter, r *http.Request, c OpCaller) {
	rev, err := strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	if err != nil {
		writeResultError(w, "Reload the page before changing the policy.")
		return
	}
	var res ops.HubUpdateCheckResult
	if err := c.CallOp(r, ops.OpHubUpdatePolicy, ops.HubUpdatePolicyArgs{Enabled: r.PostFormValue("enabled") == "on", Window: r.PostFormValue("window"), Channel: r.PostFormValue("channel"), Revision: rev}, &res); err != nil {
		writeResultError(w, failureText(err))
		return
	}
	// Reload to show the saved revision, so a second edit is not based on an
	// obsolete hidden value. HTMX follows HX-Redirect as a full navigation.
	w.Header().Set("HX-Redirect", pathAdminUpdates)
	http.Redirect(w, r, pathAdminUpdates, http.StatusSeeOther)
}

func init() {
	RegisterPage(Page{Pattern: pathAdminUpdates, Title: "Updates", Template: "updates.html", Data: adminPageData(updatesData)})
	RegisterNav(NavItem{Label: "Updates", Path: pathAdminUpdates, Order: adminNavOrder + 12, Roles: []authz.Role{authz.RoleAdmin}, Group: NavGroupAdmin, Icon: "i-settings"})
	RegisterRoute(Route{Pattern: "POST " + pathAdminUpdates + "/hub", Handler: adminRoute(handleHubUpdate)})
	RegisterRoute(Route{Pattern: "POST " + pathUpdatesCheck, Handler: adminRoute(handleUpdateCheckNow)})
	RegisterRoute(Route{Pattern: "POST " + pathAdminUpdates + "/policy", Handler: adminRoute(handleHubUpdatePolicy)})
}
