package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

const usagePath = "/usage"

type usageView struct {
	Label, URL string
	Current    bool
}

type usagePage struct {
	Section, WorkerID, BucketID, Kind, NextURL, RefreshURL, ForUser, Notice string
	Private, Admin                                                          bool
	Buckets                                                                 []ops.UsageBucket
	Workers                                                                 []ops.WorkerInfo
	Rows                                                                    ops.UsageListResult
	Accounts                                                                []usageAccountCard
	Views, SessionViews                                                     []usageView
	BucketCards                                                             []usageBucketCard
	DomainSlots                                                             []usageDomainSlotView
	DomainUnavailable, AllowRepublish                                       bool
	Preview                                                                 usagePreview
}

type usageAccountCard struct {
	Account   ops.UsageAccountRow
	Observed  string
	Freshness string
	Windows   []usageWindowView
}

type usageWindowView struct {
	Scope, Period, Used, Pace, Reset, Tone     string
	Elapsed, Projected, Unit, RemainingPercent string
}

func (p usagePage) At(ms int64) string { return p.Time(&ms) }

func (p usagePage) Time(ms *int64) string {
	if ms == nil || *ms == 0 {
		return "Unknown"
	}
	return time.UnixMilli(*ms).UTC().Format("Jan 2, 2006 15:04 UTC")
}

func (p usagePage) Millis(ms int64) string { return p.Time(&ms) }

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

func (p usagePage) Dollars(v *float64) string {
	if v == nil {
		return "Unknown"
	}
	return fmt.Sprintf("$%.4f", *v)
}

func (p usagePage) Duration(v *int64) string {
	if v == nil {
		return "Unknown"
	}
	return (time.Duration(*v) * time.Millisecond).Round(time.Second).String()
}

func (p usagePage) Period(kind string, ms *int64) string {
	label := usageLabel(kind)
	if ms == nil {
		return label
	}
	hours := float64(*ms) / 3600000
	if hours >= 24 && *ms%86400000 == 0 {
		return fmt.Sprintf("%g days · %s", hours/24, label)
	}
	if hours >= 1 {
		return fmt.Sprintf("%g hours · %s", hours, label)
	}
	return fmt.Sprintf("%g minutes · %s", float64(*ms)/60000, label)
}

func usageLabel(value string) string {
	if value == "" {
		return "Unknown"
	}
	v := strings.NewReplacer("_", " ", "-", " ").Replace(value)
	first, size := utf8.DecodeRuneInString(v)
	return string(unicode.ToUpper(first)) + v[size:]
}

func (p usagePage) Label(value string) string { return usageLabel(value) }

func (p usagePage) Access(value string) string {
	switch value {
	case "api":
		return "API credential"
	case "mixed":
		return "Multiple access methods"
	default:
		return usageLabel(value)
	}
}

func (p usagePage) Harness(value string) string {
	switch value {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "gemini":
		return "Gemini CLI"
	case "opencode":
		return "OpenCode"
	case "qwen":
		return "Qwen Code"
	case "cline":
		return "Cline"
	default:
		return usageLabel(value)
	}
}

type usageFact struct{ Label, Value string }

func (p usagePage) Counters(c ops.UsageCounters) []usageFact {
	facts := []usageFact{
		{"Input tokens", p.Number(c.InputTokens)},
		{"Output tokens", p.Number(c.OutputTokens)},
		{"Cache read / write", p.Number(c.CacheReadTokens) + " / " + p.Number(c.CacheWriteTokens)},
		{"Reasoning tokens", p.Number(c.ReasoningTokens)},
		{"Context / limit", p.Number(c.ContextTokens) + " / " + p.Number(c.ContextLimit)},
		{"Duration", p.Duration(c.DurationMS)},
		{"Reported cost", p.Dollars(c.CostUSD)},
		{"Estimated cost", p.Dollars(c.EstimatedCostUSD)},
		{"Tasks / subagents", p.Number(c.Tasks) + " / " + p.Number(c.Subagents)},
	}
	if c.APIDurationMS != nil {
		facts = append(facts, usageFact{"API wait", p.Duration(c.APIDurationMS)})
	}
	if c.LinesAdded != nil || c.LinesRemoved != nil {
		facts = append(facts, usageFact{"Lines added / removed", p.Number(c.LinesAdded) + " / " + p.Number(c.LinesRemoved)})
	}
	if c.CacheRequests != nil || c.CacheMisses != nil {
		facts = append(facts, usageFact{"Main conversation cache requests / misses", p.Number(c.CacheRequests) + " / " + p.Number(c.CacheMisses)})
	}
	if c.CacheHitRatio != nil {
		percent := 100 * *c.CacheHitRatio
		facts = append(facts, usageFact{"Main conversation cache hit ratio", p.Percent(&percent)})
	}
	return facts
}

func (p usagePage) WorkerName(id string) string {
	for _, w := range p.Workers {
		if w.ID != id {
			continue
		}
		if p.Admin && w.Owner != "" {
			return w.Name + " · " + w.Owner
		}
		return w.Name
	}
	if id == "" {
		return "Unknown"
	}
	return id
}

func (p usagePage) BucketName(id string) string {
	for _, b := range p.Buckets {
		if b.ID == id {
			return b.Label
		}
	}
	return "Unassigned"
}

func (p usagePage) WorkerStatus(status string) string {
	switch status {
	case "ready":
		return "Monitoring active"
	case "waiting":
		return "Waiting for telemetry"
	case "registration_required":
		return "Monitoring acceptance required"
	case "unavailable":
		return "Collection temporarily unavailable"
	case "unsupported":
		return "Worker update required"
	default:
		return usageLabel(status)
	}
}

func (p usagePage) CollectorStatus(status string) string {
	switch status {
	case "ready":
		return "Ready"
	case "passive":
		return "Waiting for native observations"
	case "rate_limited", "backoff":
		return "Waiting to retry"
	case "querying", "leased":
		return "Query in progress"
	case "unavailable", "offline", "unsupported", "registration_required":
		return p.WorkerStatus(status)
	default:
		return usageLabel(status)
	}
}

func usageURL(section, worker, bucket, after string) string {
	q := url.Values{}
	if section != "overview" {
		q.Set("view", section)
	}
	if worker != "" {
		q.Set("worker", worker)
	}
	if bucket != "" {
		q.Set("bucket", bucket)
	}
	if after != "" {
		q.Set("after", after)
	}
	if len(q) == 0 {
		return usagePath
	}
	return usagePath + "?" + q.Encode()
}

func usagePrivate(r *http.Request) bool {
	s, ok := session.FromContext(r.Context())
	return ok && s.ActAs == nil
}

func usagePageData(r *http.Request, c OpCaller, u pageUser) (any, error) {
	q := r.URL.Query()
	section := q.Get("view")
	switch section {
	case "", "overview":
		section = "overview"
	case "sessions", "history", "events", "accounts", "workers", "buckets":
	default:
		return nil, ErrNotFound()
	}

	p := usagePage{
		Section: section, WorkerID: q.Get("worker"), BucketID: q.Get("bucket"),
		ForUser: u.ID, Admin: u.Admin, Notice: ops.UsageDisclosure, Private: usagePrivate(r),
	}
	if p.Private {
		var result ops.UsageBucketListResult
		if err := c.CallOp(r, ops.OpUsageBucketList, ops.UsageBucketListArgs{}, &result); err != nil {
			return nil, err
		}
		p.Buckets, p.AllowRepublish = result.Buckets, result.AllowRepublish
	}
	sessionView := section == "sessions" || section == "events" || section == "history"
	p.Kind = "provider"
	if sessionView {
		p.Kind = "session"
	}
	if !p.Private || section == "workers" || section == "buckets" {
		p.BucketID = ""
	}
	p.Views = []usageView{
		{"Subscriptions", usageURL("overview", p.WorkerID, "", ""), section == "overview"},
		{"Accounts", usageURL("accounts", p.WorkerID, "", ""), section == "accounts"},
		{"Sessions", usageURL("sessions", p.WorkerID, "", ""), sessionView},
		{"Workers", usageURL("workers", p.WorkerID, "", ""), section == "workers"},
	}
	if p.Private {
		p.Views = append(p.Views, usageView{"Token buckets", usagePath + "?view=buckets", section == "buckets"})
	}
	if sessionView {
		for _, v := range []struct{ section, label string }{{"sessions", "Sessions"}, {"events", "Activity"}, {"history", "Hourly snapshots"}} {
			p.SessionViews = append(p.SessionViews, usageView{v.label, usageURL(v.section, p.WorkerID, p.BucketID, ""), section == v.section})
		}
	}
	if section == "buckets" {
		if p.Private {
			var domain ops.UsageNominationListResult
			if p.Admin {
				if err := c.CallOp(r, ops.OpUsageNominationList, ops.UsageNominationListArgs{}, &domain); err != nil {
					p.DomainUnavailable = true
				}
			}
			p.buildBuckets(domain.Domain)
			var doc ops.UsageMeters
			if err := c.CallOp(r, ops.OpUsageReadback, ops.UsageReadbackArgs{}, &doc); err != nil {
				p.Preview.Unavailable = true
			} else {
				p.Preview = buildUsagePreview(doc)
			}
		}
		return p, nil
	}
	var workers ops.WorkersListResult
	if err := c.CallOp(r, ops.OpWorkersList, ops.WorkersListArgs{All: true}, &workers); err != nil {
		return nil, err
	}
	for _, w := range workers.Workers {
		if !w.Pending {
			p.Workers = append(p.Workers, w)
		}
	}

	args := ops.UsageListArgs{Section: section, WorkerID: p.WorkerID, BucketID: p.BucketID, After: q.Get("after"), Limit: 50}
	if err := c.CallOp(r, ops.OpUsageList, args, &p.Rows); err != nil {
		return nil, err
	}
	accounts, next := p.Rows.Accounts, p.Rows.Next
	if section == "overview" {
		var subscriptions ops.UsageListResult
		args.Section, args.SubscriptionOnly = "accounts", true
		if err := c.CallOp(r, ops.OpUsageList, args, &subscriptions); err != nil {
			return nil, err
		}
		accounts, next = subscriptions.Accounts, subscriptions.Next
		p.RefreshURL = usageURL(section, args.WorkerID, args.BucketID, args.After)
	}
	if next != "" {
		p.NextURL = usageURL(section, p.WorkerID, p.BucketID, next)
	}
	for _, a := range accounts {
		p.Accounts = append(p.Accounts, p.accountCard(a))
	}
	return p, nil
}

func (p usagePage) accountCard(a ops.UsageAccountRow) usageAccountCard {
	now := time.UnixMilli(p.Rows.AsOf)
	stale := now.Sub(time.UnixMilli(a.ObservedAt)) > 15*time.Minute
	card := usageAccountCard{Account: a, Observed: p.Time(&a.ObservedAt), Freshness: "Fresh"}
	if stale {
		card.Freshness = "Stale"
	}
	if a.ObservedAt == 0 {
		card.Freshness = "Unknown"
	}
	if a.AuthMode != "subscription" {
		return card
	}
	for _, w := range a.Windows {
		view := usageWindowView{Scope: w.Scope, Period: p.Period(w.Period, w.DurationMS), Used: p.Percent(w.UsedPercent), Reset: p.Time(w.ResetsAt), Unit: w.Unit}
		view.RemainingPercent = "Unknown"
		if w.UsedPercent != nil {
			remaining := 100 - *w.UsedPercent
			view.RemainingPercent = p.Percent(&remaining)
		}

		switch w.Pace(now) {
		case "ahead":
			view.Pace, view.Tone = "Ahead of pace", "warning"
		case "below":
			view.Pace, view.Tone = "Within pace", "positive"
		case "on_pace":
			view.Pace, view.Tone = "On pace", "info"
		default:
			view.Pace, view.Tone = "Unknown pace", "info"
		}
		if stale || w.ResetsAt != nil && *w.ResetsAt <= now.UnixMilli() {
			view.Pace, view.Tone = "Stale window", "warning"
		} else if view.Pace != "Unknown pace" {
			elapsed := 100 * (1 - float64(*w.ResetsAt-now.UnixMilli())/float64(*w.DurationMS))
			view.Elapsed = p.Percent(&elapsed)
			// A linear projection is unstable at the beginning of a window.
			if elapsed >= 5 {
				projected := 100 * *w.UsedPercent / elapsed
				view.Projected = p.Percent(&projected)
			}
		}
		card.Windows = append(card.Windows, view)
	}
	return card
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
			writeResultError(w, failureText(err))
			return
		}
	case "account":
		if !usagePrivate(r) {
			writeResultError(w, "Account names and order are available in your own signed-in account.")
			return
		}
		position, err := usagePosition(r.PostFormValue("position"))
		if err != nil {
			writeResultError(w, err.Error())
			return
		}
		args := ops.UsageAccountSetArgs{AccountID: r.PostFormValue("account"), Label: r.PostFormValue("label"), Position: position}
		if err := c.CallOp(r, ops.OpUsageAccountSet, args, nil); err != nil {
			writeResultError(w, failureText(err))
			return
		}
	case "collector":
		if !usagePrivate(r) {
			writeResultError(w, "Collector settings are available in your own signed-in account.")
			return
		}
		worker, sid, _ := strings.Cut(r.PostFormValue("source"), "|")
		var res ops.UsageCollector
		err := c.CallOp(r, ops.OpUsageCollectorSet, ops.UsageCollectorSetArgs{AccountID: r.PostFormValue("account"), WorkerID: worker, SessionID: sid, Start: r.PostFormValue("run") == "native"}, &res)
		if err != nil {
			writeResultError(w, failureText(err))
			return
		}
	case "bucket":
		if !usagePrivate(r) {
			writeResultError(w, "Token buckets are available in your own signed-in account.")
			return
		}
		args, err := usageBucketForm(r)
		if err != nil {
			writeResultError(w, err.Error())
			return
		}
		var res ops.UsageBucketSetResult
		if err := c.CallOp(r, ops.OpUsageBucketSet, args, &res); err != nil {
			writeResultError(w, failureText(err))
			return
		}
	case "nominate":
		if !usagePrivate(r) {
			writeResultError(w, "Publishing buckets is available in your own signed-in account.")
			return
		}
		args, err := usageNominationForm(r)
		if err != nil {
			writeResultError(w, err.Error())
			return
		}
		if args.Scope == ops.UsageScopeDomain && !u.Admin {
			writeResultError(w, "Only administrators can publish to every Worker.")
			return
		}
		var res ops.UsageNominationSetResult
		if err := c.CallOp(r, ops.OpUsageNominationSet, args, &res); err != nil {
			writeResultError(w, failureText(err))
			return
		}
	case "publication":
		if !usagePrivate(r) {
			writeResultError(w, "Publication consent is available in your own signed-in account.")
			return
		}
		args := ops.UsagePublicationSetArgs{AllowRepublish: r.PostFormValue("allow_republish") == "yes"}
		if err := c.CallOp(r, ops.OpUsagePublicationSet, args, nil); err != nil {
			writeResultError(w, failureText(err))
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	location := usageURL(view, r.PostFormValue("filter_worker"), r.PostFormValue("filter_bucket"), "")
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", location)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func usageBucketForm(r *http.Request) (ops.UsageBucketSetArgs, error) {
	a := ops.UsageBucketSetArgs{Action: r.PostFormValue("action"), Kind: r.PostFormValue("kind"), BucketID: r.PostFormValue("bucket"), Label: r.PostFormValue("label")}
	position, err := usagePosition(r.PostFormValue("position"))
	if err != nil {
		return a, err
	}
	a.Position = position
	if a.Action != "assign" {
		return a, nil
	}
	switch r.PostFormValue("selection") {
	case "selected":
		a.Members = r.PostForm["member"]
		if len(a.Members) == 0 {
			return a, fmt.Errorf("select at least one row to move")
		}
	case "worker":
		if id := strings.TrimSpace(r.PostFormValue("worker")); id != "" {
			a.WorkerIDs = []string{id}
		} else {
			return a, fmt.Errorf("choose a Worker to move all its members")
		}
	case "all":
		a.All = true
	default:
		return a, fmt.Errorf("choose selected rows, a Worker, or all Workers")
	}
	return a, nil
}

func usagePosition(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 || v > 1000000 {
		return 0, fmt.Errorf("enter a whole number from 0 to 1,000,000 for the order")
	}
	return v, nil
}

func init() {
	RegisterPage(Page{Pattern: usagePath, Title: "Usage", Template: "usage.html", Data: userPageData(usagePageData)})
	RegisterNav(NavItem{Label: "Usage", Path: usagePath, Order: 125, Group: NavGroupWorkspace, Icon: "i-audit"})
	RegisterRoute(Route{Pattern: "POST " + usagePath + "/{action}", Handler: userRoute(usagePost)})
}
