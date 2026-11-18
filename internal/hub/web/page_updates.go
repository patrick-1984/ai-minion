package web

// Administration > Updates (v0.9.0, A10): the Hub's release, the newest one
// in its update channel, the Hub's own update (Docker, opt-in) and the
// Workers that are older than the Hub.

import (
	"fmt"
	"net/http"
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
	if err := c.CallOp(r, ops.OpHubUpdateCheck, ops.HubUpdateCheckArgs{Refresh: true}, &res); err != nil {
		writeResultError(w, failureText(err))
		return
	}
	var text string
	switch {
	case !res.Refreshed:
		text = "Checked less than a minute ago; this is that answer."
	case res.CheckError != "":
		text = "The check failed: " + res.CheckError + "."
	case res.Latest == "":
		text = "Checked: the release check is off (update_check) or found no release in the channel."
	case res.Newer:
		text = fmt.Sprintf("Checked: %s is newer than this Hub (%s) in the channel %s.", res.Latest, res.Current, res.Channel)
	default:
		text = fmt.Sprintf("Checked: %s is the newest release in the channel %s; this Hub is up to date.", res.Latest, res.Channel)
	}
	done(w, r, text, ops.WriteResult{})
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
	RegisterRoute(Route{Pattern: "POST " + pathAdminUpdates + "/check", Handler: adminRoute(handleUpdateCheckNow)})
}
