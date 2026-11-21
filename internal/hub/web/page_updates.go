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
	// PollURL is set while a check runs (or the channel's answer is pending):
	// the panel then reloads itself from it every 2 seconds. Empty when the
	// check is done or the bound (updatePollMax polls) is reached.
	PollURL string
	// Note is a line under the facts (Check now: why nothing was started).
	Note string
}

const (
	pathUpdatesPanel = pathAdminUpdates + "/panel"
	// updatePollMax bounds the polling (2 s each): a hung check does not poll forever.
	updatePollMax = 90
)

var updatesFragments = fragments("updates.html")

// withPoll sets PollURL while the check is not done; n counts the polls so far.
func (p *UpdatesPage) withPoll(n int) {
	c := p.Check
	pending := c.CheckedChannel != "" && c.CheckedChannel != c.Channel
	if (c.Checking || pending) && n < updatePollMax {
		p.PollURL = fmt.Sprintf("%s?n=%d", pathUpdatesPanel, n+1)
	}
}

func updatesData(r *http.Request, c OpCaller) (any, error) {
	var res UpdatesPage
	if err := c.CallOp(r, ops.OpHubUpdateCheck, ops.HubUpdateCheckArgs{}, &res.Check); err != nil {
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339, res.Check.CheckedAt); err == nil {
		res.CheckedAt = t.UTC().Format("2006-01-02 15:04 UTC")
	}
	res.withPoll(0)
	return res, nil
}

// handleUpdatesPanel serves the Hub panel (GET), which polls itself while a check runs.
func handleUpdatesPanel(w http.ResponseWriter, r *http.Request, c OpCaller) {
	v, err := updatesData(r, c)
	if err != nil {
		w.WriteHeader(stopPolling)
		return
	}
	res := v.(UpdatesPage)
	res.PollURL = ""
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	res.withPoll(max(n, 1))
	writeFragmentTemplate(w, updatesFragments(), "hub_panel", res)
}

// handleUpdateCheckNow asks GitHub now ("Check now").
func handleUpdateCheckNow(w http.ResponseWriter, r *http.Request, c OpCaller) {
	var res ops.HubUpdateCheckResult
	if err := c.CallOp(r, ops.OpHubUpdateRefresh, ops.HubUpdateRefreshArgs{}, &res); err != nil {
		writeResultError(w, failureText(err))
		return
	}
	page := UpdatesPage{Check: res}
	if t, err := time.Parse(time.RFC3339, res.CheckedAt); err == nil {
		page.CheckedAt = t.UTC().Format("2006-01-02 15:04 UTC")
	}
	if !res.Refreshed {
		page.Note = "Not checked again: the last check was less than a minute ago, or the release check is off (update_check). The page shows the last answer."
	}
	page.withPoll(0)
	writeFragmentTemplate(w, updatesFragments(), "hub_panel", page)
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
	RegisterRoute(Route{Pattern: "GET " + pathUpdatesPanel, Handler: adminRoute(handleUpdatesPanel)})
	RegisterRoute(Route{Pattern: "POST " + pathAdminUpdates + "/hub", Handler: adminRoute(handleHubUpdate)})
	RegisterRoute(Route{Pattern: "POST " + pathAdminUpdates + "/check", Handler: adminRoute(handleUpdateCheckNow)})
}
