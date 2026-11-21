package web

// Administration > Updates (v0.9.0, A10): the Hub's release, the newest one
// in its update channel, the Hub's own update (Docker, opt-in) and the
// Workers that are older than the Hub.

import (
	"fmt"
	"net/http"
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
}

func updatesData(r *http.Request, c OpCaller) (any, error) {
	var res UpdatesPage
	if err := c.CallOp(r, ops.OpHubUpdateCheck, ops.HubUpdateCheckArgs{}, &res.Check); err != nil {
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339, res.Check.CheckedAt); err == nil {
		res.CheckedAt = t.UTC().Format("2006-01-02 15:04 UTC")
	}
	return res, nil
}

// handleUpdateCheckNow asks GitHub now ("Check now").
func handleUpdateCheckNow(w http.ResponseWriter, r *http.Request, c OpCaller) {
	var res ops.HubUpdateCheckResult
	if err := c.CallOp(r, ops.OpHubUpdateRefresh, ops.HubUpdateRefreshArgs{}, &res); err != nil {
		writeResultError(w, failureText(err))
		return
	}
	text := "Not checked again: the last check was less than a minute ago, or the release check is off (update_check). The page shows the last answer."
	if res.Refreshed {
		text = "Checking GitHub now. Reload this page in a few seconds for the answer."
	}
	done(w, r, text, ops.WriteResult{})
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
	RegisterRoute(Route{Pattern: "POST " + pathAdminUpdates + "/check", Handler: adminRoute(handleUpdateCheckNow)})
	RegisterRoute(Route{Pattern: "POST " + pathAdminUpdates + "/policy", Handler: adminRoute(handleHubUpdatePolicy)})
}
