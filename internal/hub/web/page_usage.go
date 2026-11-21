package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

const usagePath = "/usage"

type usagePage struct {
	Section, WorkerID, BucketID, Kind, NextURL, ForUser, Notice string
	Private                                                     bool
	Buckets                                                     []ops.UsageBucket
	Rows                                                        ops.UsageListResult
	Windows                                                     []usageAccountCard
}

type usageAccountCard struct {
	Account  ops.UsageAccountRow
	Observed string
	Windows  []usageWindowView
}
type usageWindowView struct{ Scope, Period, Used, Pace, Reset, Tone string }

func (p usagePage) Time(ms *int64) string {
	if ms == nil {
		return "Unknown"
	}
	return time.UnixMilli(*ms).UTC().Format("2006-01-02 15:04 UTC")
}

func (p usagePage) Percent(n *float64) string {
	if n == nil {
		return "Unknown"
	}
	return fmt.Sprintf("%.1f%%", *n)
}

func (p usagePage) Number(n *int64) string {
	if n == nil {
		return "Unknown"
	}
	return strconv.FormatInt(*n, 10)
}

func (p usagePage) Period(kind string, ms *int64) string {
	if ms == nil {
		return kind
	}
	hours := float64(*ms) / 3600000
	if hours >= 24 && *ms%86400000 == 0 {
		return fmt.Sprintf("%g days (%s)", hours/24, kind)
	}
	if hours >= 1 {
		return fmt.Sprintf("%g hours (%s)", hours, kind)
	}
	return fmt.Sprintf("%g minutes (%s)", float64(*ms)/60000, kind)
}

func usagePageData(r *http.Request, c OpCaller, u pageUser) (any, error) {
	q := r.URL.Query()
	section := q.Get("view")
	switch section {
	case "", "overview":
		section = "overview"
	case "sessions", "accounts", "workers", "buckets":
	default:
		return nil, ErrNotFound()
	}
	p := usagePage{Section: section, WorkerID: q.Get("worker"), BucketID: q.Get("bucket"), ForUser: u.ID, Notice: ops.UsageDisclosure, Private: true}
	if sess, ok := session.FromContext(r.Context()); ok && sess.ActAs != nil {
		p.Private = false
	}
	if p.Private {
		var buckets ops.UsageBucketListResult
		if err := c.CallOp(r, ops.OpUsageBucketList, ops.UsageBucketListArgs{}, &buckets); err != nil {
			return nil, err
		}
		p.Buckets = buckets.Buckets
	} else {
		p.BucketID = ""
	}
	if section == "buckets" {
		return p, nil
	}
	p.Kind = "provider"
	if section == "sessions" {
		p.Kind = "session"
	}
	args := ops.UsageListArgs{Section: section, WorkerID: p.WorkerID, BucketID: p.BucketID, After: q.Get("after"), Limit: 50}
	if section == "workers" {
		args.BucketID = ""
	}
	if err := c.CallOp(r, ops.OpUsageList, args, &p.Rows); err != nil {
		return nil, err
	}
	if p.Rows.Next != "" {
		q.Set("after", p.Rows.Next)
		p.NextURL = usagePath + "?" + q.Encode()
	}
	for _, a := range p.Rows.Accounts {
		card := usageAccountCard{Account: a, Observed: p.Time(&a.ObservedAt)}
		now := time.UnixMilli(p.Rows.AsOf)
		for _, w := range a.Windows {
			pace := w.Pace(now)
			if now.Sub(time.UnixMilli(a.ObservedAt)) > 15*time.Minute {
				pace = "stale"
			}
			label := map[string]string{"ahead": "Using faster", "below": "Using slower", "on_pace": "On pace", "unknown": "Unknown", "stale": "Stale"}[pace]
			card.Windows = append(card.Windows, usageWindowView{w.Scope, p.Period(w.Period, w.DurationMS), p.Percent(w.UsedPercent), label, p.Time(w.ResetsAt), map[string]string{"ahead": "warning", "on_pace": "success", "below": "success"}[pace]})
		}
		p.Windows = append(p.Windows, card)
	}
	return p, nil
}

func usagePost(w http.ResponseWriter, r *http.Request, c OpCaller, u pageUser) {
	if r.PostFormValue("for_user") != u.ID {
		writeResultError(w, "The signed-in account changed. Reload the Usage page.")
		return
	}
	view := r.PostFormValue("view")
	switch view {
	case "accounts", "sessions", "workers", "buckets":
	default:
		view = "overview"
	}
	switch r.PathValue("action") {
	case "scan":
		var report ops.UsageReport
		if err := c.CallOp(r, ops.OpWorkerUsage, ops.WorkerUsageArgs{WorkerID: r.PostFormValue("worker"), Inventory: true}, &report); err != nil {
			writeResultError(w, err.Error())
			return
		}
	case "bucket":
		position := 0
		if raw := r.PostFormValue("position"); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil {
				writeResultError(w, "Enter a whole number for the order.")
				return
			}
			position = v
		}
		args := ops.UsageBucketSetArgs{Action: r.PostFormValue("action"), Kind: r.PostFormValue("kind"), BucketID: r.PostFormValue("bucket"), Label: r.PostFormValue("label"), Position: position, Members: r.PostForm["member"], All: r.PostFormValue("all") == "on"}
		if id := strings.TrimSpace(r.PostFormValue("worker")); id != "" {
			args.WorkerIDs = []string{id}
		}
		var res ops.UsageBucketSetResult
		if err := c.CallOp(r, ops.OpUsageBucketSet, args, &res); err != nil {
			writeResultError(w, err.Error())
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	location := usagePath + "?" + url.Values{"view": {view}}.Encode()
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", location)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func init() {
	RegisterPage(Page{Pattern: usagePath, Title: "Usage", Template: "usage.html", Data: userPageData(usagePageData)})
	RegisterNav(NavItem{Label: "Usage", Path: usagePath, Order: 125, Group: NavGroupWorkspace, Icon: "i-audit"})
	RegisterRoute(Route{Pattern: "POST " + usagePath + "/{action}", Handler: userRoute(usagePost)})
}
