package web

// The home page: the signed-in user's Workers and sessions, updated live
// (Story 5.4; AD-21, AD-18, AD-17; R36, R46-R48, UX-DR5).
//
// The page is server-rendered. It shows two tables, Workers (name, online,
// kind badge private/infrastructure, version) and sessions (name, kind, state,
// task), and opens one SSE connection (htmx SSE extension) to /live/dashboard.
// That stream sends the whole "dashboard" fragment again after a change the
// Hub reports (one fixed batching window per burst, so a feed that never
// pauses still renders every window) and on a slow periodic reconciliation
// for changes the event bus does not carry (a Worker renamed, removed or
// reassigned while offline). A fragment is sent only when it differs from the
// last one sent, so a screen change that the lists do not show costs nothing.
// The stream is bounded on the original outer request: an announced request
// body is read under a deadline before anything is subscribed, every write to
// the browser has a deadline, and a write blocked on a client that does not
// read is cut as soon as the feed behind the stream ends.
//
// The page never reads a store. Everything comes through the [LiveSource]
// port, whose production implementation ([NewHandlerSource]) calls the
// Hub's own REST API in process as the signed-in user: workers_list,
// GET /api/v1/sessions and the GET /api/v1/events feed. Visibility (a user
// sees only their own Workers and sessions, an admin may widen the list) is
// therefore decided by the API (AD-10, AD-11), never by this file; the
// "mine / all users" switch only chooses what to ask for and is shown to an
// admin who acts as themselves.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Paths of the home page.
const (
	pathHome          = "/{$}"
	pathLiveDashboard = "/live/dashboard"
	pathWorkers       = "/workers"
	pathSessions      = "/sessions"
	pathLiveWorkers   = "/live/workers"
	pathLiveSessions  = "/live/sessions"

	apiSessionsPath = "/api/v1/sessions"
	apiEventsPath   = "/api/v1/events"
)

// Scope values of the "mine / all users" switch.
const (
	scopeMine = "mine"
	scopeAll  = "all"
)

// Timing of the live stream.
const (
	// defaultLiveDebounce is the batching window that merges a burst of
	// changes into one render.
	defaultLiveDebounce = 250 * time.Millisecond
	// defaultLiveHeartbeat is the keep-alive comment interval.
	defaultLiveHeartbeat = 15 * time.Second
	// defaultLiveReconcile is the period of the snapshot that does not wait
	// for an event.
	defaultLiveReconcile = 10 * time.Second
	// defaultLiveWriteTimeout bounds one write to the browser.
	defaultLiveWriteTimeout = 30 * time.Second
	// defaultLiveBodyTimeout bounds an announced request body.
	defaultLiveBodyTimeout = 30 * time.Second
	liveRetry              = 3 * time.Second
)

// WorkerRow is one Worker of the dashboard.
type WorkerRow struct {
	ID     string
	Name   string
	Owner  string
	Kind   string
	Online bool
	// Pending (F-02) is true while the Worker waits for approval. Fingerprint
	// and EnrollAddr are what the person who approves it compares; ApproveURL and
	// RemoveURL are the posts of the two buttons (filled by [newFilteredView]
	// for a pending Worker only).
	Pending     bool
	Fingerprint string
	EnrollAddr  string
	ApproveURL  string
	RemoveURL   string
	// NeverConnected is true while the Worker has never connected to the Hub;
	// RepairURL is then the post of the "Re-issue pairing" button (design
	// 12.3), filled by [newWorkerView] for the owner or an administrator only.
	NeverConnected bool
	RepairURL      string
	// Version is the protocol version of the current connection; empty while
	// the Worker is offline.
	Version string
	// ShowOwner is set when the owner column is shown ("all users").
	ShowOwner bool
	// SessionCount, SessionsURL and NewURL are filled by [newDashboardView]:
	// the number of sessions the list shows for this Worker, the session list
	// of this Worker, and the route that creates a session on it.
	SessionCount int
	SessionsURL  string
	NewURL       string
	// URL is the Worker's own page (its detail view), by canonical id, in the same
	// scope as the list it is shown in. The name of a Worker is not unique and never
	// addresses it.
	URL string
	// Sessions are this Worker's sessions of the same authorised snapshot that
	// feeds the session list (at most [maxWorkerSessions]); Disclose says that the
	// row offers them as links that open the session inspector. Both are filled by
	// [newFilteredView] and [DashboardView.discloseSessions]; nothing is added that
	// the session list does not show.
	Sessions []SessionRow
	Disclose bool
}

// maxWorkerSessions bounds the sessions a Workers row lists; the count link
// leads to the full list of that Worker.
const maxWorkerSessions = 25

// SessionRow is one session of the dashboard.
type SessionRow struct {
	WorkerID string
	Worker   string
	Owner    string
	Name     string
	Kind     string
	State    string
	Task     string
	// Online is false while the session's Worker is offline; the state is the
	// last one known.
	Online    bool
	ShowOwner bool
	// Path is the session view, WorkerURL the page of the session's Worker (by
	// canonical id, in the same scope) and StateLabel the state as people read it;
	// all three are filled by [newDashboardView].
	Path       string
	WorkerURL  string
	StateLabel string
	// HideWorker is set on the Worker's own page, where the Worker column would
	// repeat the heading.
	HideWorker bool
}

// Dashboard is the data of the home page as a [LiveSource] returns it.
type Dashboard struct {
	Workers  []WorkerRow
	Sessions []SessionRow
	// Notice is a sentence shown above the tables, for example when this Hub
	// does not serve the Worker list or the session list.
	Notice string
}

// ListFilter is the search of the Workers and sessions pages. Every field is
// optional; an empty filter shows everything the scope allows. The filter only
// narrows a list the API already limited to what the caller may see.
type ListFilter struct {
	// Query matches names, tasks, Worker names and owners, ignoring case.
	Query string
	// Status is "online" or "offline" for Workers; a session state or
	// "worker-offline" for sessions.
	Status string
	Kind   string
	Owner  string
	// Worker limits sessions to one Worker (its id).
	Worker string
}

const maxFilterRunes = 128

// parseListFilter reads the filter of a list page from the query string.
func parseListFilter(r *http.Request) ListFilter {
	q := r.URL.Query()
	get := func(k string) string { return truncateRunes(strings.TrimSpace(q.Get(k)), maxFilterRunes) }
	return ListFilter{Query: get("q"), Status: get("status"), Kind: get("kind"), Owner: get("owner"), Worker: get("worker")}
}

// Active reports whether the filter narrows anything.
func (f ListFilter) Active() bool { return f != ListFilter{} }

// Values returns the filter as query parameters (empty fields left out).
func (f ListFilter) Values() url.Values {
	v := url.Values{}
	for k, x := range map[string]string{"q": f.Query, "status": f.Status, "kind": f.Kind, "owner": f.Owner, "worker": f.Worker} {
		if x != "" {
			v.Set(k, x)
		}
	}
	return v
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func (f ListFilter) matchWorker(w WorkerRow) bool {
	if f.Query != "" && !containsFold(w.Name, f.Query) && !containsFold(w.Owner, f.Query) {
		return false
	}
	switch f.Status {
	case "online":
		if !w.Online {
			return false
		}
	case "offline":
		if w.Online {
			return false
		}
	}
	return (f.Kind == "" || w.Kind == f.Kind) && (f.Owner == "" || w.Owner == f.Owner) && (f.Worker == "" || w.ID == f.Worker)
}

func (f ListFilter) matchSession(x SessionRow) bool {
	if f.Query != "" && !containsFold(x.Name, f.Query) && !containsFold(x.Task, f.Query) &&
		!containsFold(x.Worker, f.Query) && !containsFold(x.Owner, f.Query) {
		return false
	}
	switch f.Status {
	case "":
	case "worker-offline":
		if x.Online {
			return false
		}
	default:
		if x.State != f.Status {
			return false
		}
	}
	return (f.Kind == "" || x.Kind == f.Kind) && (f.Owner == "" || x.Owner == f.Owner) && (f.Worker == "" || x.WorkerID == f.Worker)
}

// stateLabel is a session state as people read it. A state the page does not
// know is shown as the Worker sent it.
func stateLabel(state string) string {
	switch state {
	case ops.PaneStateStable:
		return "Idle"
	case ops.PaneStateBusy:
		return "Working"
	case ops.PaneStateDialog:
		return "Waiting for an answer"
	case ops.PaneStateUnknown, "":
		return "Unknown"
	}
	return state
}

// AttentionItem is one thing that needs a look: a session that waits for an
// answer, or a Worker that is offline.
type AttentionItem struct {
	Text string
	// Link and LinkText are the page that handles it.
	Link     string
	LinkText string
	// WorkerID, Session and Target identify the session of a "waiting for an
	// answer" item (empty for a Worker): the link then opens the inspector.
	WorkerID string
	Session  string
	Target   string
}

// maxAttention bounds the attention list of the control center.
const maxAttention = 8

// DashboardView is what the list fragments see. Workers and Sessions are the
// rows to show (after the filter); the Total fields count the rows of the scope.
type DashboardView struct {
	Workers  []WorkerRow
	Sessions []SessionRow
	Notice   string
	// ShowOwner is true in the "all users" scope.
	ShowOwner bool
	// OnWorker is false for the lists; the Worker's own page sets it (see [WorkerView]).
	OnWorker bool
	Filter   ListFilter
	// TotalWorkers, OnlineWorkers, TotalSessions and WaitingSessions count the
	// whole scope, not the filtered rows.
	TotalWorkers    int
	OnlineWorkers   int
	PendingWorkers  int
	TotalSessions   int
	WaitingSessions int
	// Attention is built from real states only.
	Attention []AttentionItem
	// ResultsText is the one-sentence summary of the shown rows, for the
	// polite announcement after a live update.
	WorkersText  string
	SessionsText string
	// ClearURL is the list page without filters, in the same scope.
	ClearURL string
}

// noMatchView is what the "no_match" template sees: a list that the filters
// emptied, which is not the same as a list with nothing in it.
type noMatchView struct {
	Title    string
	Scope    string
	Query    string
	Filters  string
	ClearURL string
}

// NoMatch describes the empty result of a filtered list; noun is "workers" or
// "sessions".
func (v DashboardView) NoMatch(noun string) noMatchView {
	n := noMatchView{Title: "No " + noun + " match", Scope: "Scope: Mine.", Query: v.Filter.Query, ClearURL: v.ClearURL}
	if v.ShowOwner {
		n.Scope = "Scope: All users."
	}
	var parts []string
	for _, f := range []struct{ name, val string }{{"status", v.Filter.Status}, {"kind", v.Filter.Kind}, {"owner", v.Filter.Owner}} {
		if f.val != "" {
			parts = append(parts, f.name+" "+f.val)
		}
	}
	if v.Filter.Worker != "" {
		parts = append(parts, "one worker")
	}
	n.Filters = strings.Join(parts, ", ")
	return n
}

// DashboardPage is what sessions.html (the control center) sees as .Data.
type DashboardPage struct {
	DashboardView
	// Scope is "mine" or "all".
	Scope string
	// CanFilter shows the "mine / all users" switch (admins only).
	CanFilter bool
	MineURL   string
	AllURL    string
	// LiveURL is the SSE stream of the page, in the same scope.
	LiveURL string
	// Context is the public identity context of the request (see page_inspector_context.go).
	Context string
}

// Crumbs implements [Crumbed].
func (DashboardPage) Crumbs() []Crumb { return []Crumb{{Label: "Control center"}} }

// ListPage is what workers.html and sessionlist.html see as .Data: a filtered
// list with its toolbar.
type ListPage struct {
	DashboardView
	Scope     string
	CanFilter bool
	MineURL   string
	AllURL    string
	LiveURL   string
	// Context is the public identity context of the request (see page_inspector_context.go).
	Context string
	// Path is the page itself; ClearURL is the page without filters, in the
	// same scope.
	Path     string
	ClearURL string
	Statuses []Choice
	Kinds    []Choice
	Owners   []Choice
	// WorkerName names the Worker the list is limited to, when it is.
	WorkerName string
	// IdentityNote is the certificate countdown in the header of the Workers
	// page, for an administrator; IdentityTone is "ok", "warning" or "danger".
	IdentityNote, IdentityTone string
}

// LiveSource is the port of the home page to the Hub's live state.
type LiveSource interface {
	// Snapshot returns the Workers and sessions the caller of r may see. With
	// all set an admin gets every user's; for anyone else it changes nothing.
	// A failure is an [*OpError] when the API refused the call.
	Snapshot(r *http.Request, all bool) (Dashboard, error)
	// Changes returns a channel that receives a value whenever something may
	// have changed (values coalesce; one pending value is enough). The
	// channel is closed when the feed ends, for example when access is lost
	// or the Hub restarts; the caller then ends its own stream and the browser
	// reconnects. Cancelling ctx stops the feed.
	Changes(ctx context.Context, r *http.Request, all bool) (<-chan struct{}, error)
}

// liveSource is the explicitly connected source; see [SetLiveSource].
var liveSource atomic.Pointer[sourceBox]

type sourceBox struct{ s LiveSource }

// SetLiveSource connects the home page to a source. A nil source removes it.
// Without a call the page uses the in-process REST caller that the Hub's web
// module connected with [SetAdminOps]; before either, the page answers 503.
func SetLiveSource(s LiveSource) {
	if s == nil {
		liveSource.Store(nil)
		return
	}
	liveSource.Store(&sourceBox{s: s})
}

// currentLiveSource returns the connected source, or nil.
func currentLiveSource() LiveSource {
	if b := liveSource.Load(); b != nil {
		return b.s
	}
	if b := adminOps.Load(); b != nil {
		if hc, ok := b.c.(*HandlerCaller); ok {
			return NewHandlerSource(hc)
		}
	}
	return nil
}

// dashboardScope tells whether the request asks for every user's data and
// whether the switch is offered. Only an admin who acts as themselves may
// widen the list: while an admin acts as another user the API treats the
// session as that user, so the switch would promise what it cannot give.
func dashboardScope(r *http.Request) (all, canFilter bool) {
	sess, ok := session.FromContext(r.Context())
	if !ok || sess.Role != authz.RoleAdmin || sess.ActAs != nil {
		return false, false
	}
	return r.URL.Query().Get("scope") == scopeAll, true
}

// workerRef is how the path of a Worker names it: by name while the name is
// unique in the list, else by id (an admin may see equal names of several owners).
func workerRefs(ws []WorkerRow) map[string]string {
	names := map[string]int{}
	for _, w := range ws {
		names[w.Name]++
	}
	out := make(map[string]string, len(ws))
	for _, w := range ws {
		if names[w.Name] == 1 && w.Name != "" {
			out[w.ID] = w.Name
		} else {
			out[w.ID] = w.ID
		}
	}
	return out
}

// scopedLink builds a list link with the given query. The All users scope is part
// of the link: without it the target page opens in Mine, where another owner's
// sessions are (correctly) left out and the drill-down looks empty.
func scopedLink(page string, all bool, kv ...string) string {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	if all {
		q.Set("scope", scopeAll)
	}
	if len(q) == 0 {
		return page
	}
	return page + "?" + q.Encode()
}

// newDashboardView builds the view of a snapshot with no filter.
func newDashboardView(d Dashboard, all bool) DashboardView {
	return newFilteredView(d, all, ListFilter{})
}

// newFilteredView builds the view of a snapshot and narrows the rows by f. The
// counts and the attention list describe the whole scope.
func newFilteredView(d Dashboard, all bool, f ListFilter) DashboardView {
	v := DashboardView{ShowOwner: all, Notice: d.Notice, Filter: f}
	refs := workerRefs(d.Workers)
	perWorker := map[string]int{}
	for _, x := range d.Sessions {
		perWorker[x.WorkerID]++
	}
	v.TotalWorkers, v.TotalSessions = len(d.Workers), len(d.Sessions)
	for _, w := range d.Workers {
		if w.Pending {
			v.PendingWorkers++
			w.ApproveURL, w.RemoveURL = WorkerApprovePath(w.ID), WorkerRemovePath(w.ID)
			if len(v.Attention) < maxAttention {
				v.Attention = append(v.Attention, AttentionItem{Text: "Worker " + w.Name + " is waiting for approval.", Link: scopedLink("/workers", all, "worker", w.ID), LinkText: "Review worker"})
			}
		} else if w.Online {
			v.OnlineWorkers++
		} else if len(v.Attention) < maxAttention {
			v.Attention = append(v.Attention, AttentionItem{Text: "Worker " + w.Name + " is offline.", Link: scopedLink("/workers", all, "worker", w.ID), LinkText: "View worker"})
		}
		w.ShowOwner = all
		w.SessionCount = perWorker[w.ID]
		w.SessionsURL = scopedLink("/sessions", all, "worker", w.ID)
		w.NewURL = NewSessionPath(refs[w.ID])
		w.URL = WorkerPath(w.ID, all)
		w.Sessions = nil
		if f.matchWorker(w) {
			v.Workers = append(v.Workers, w)
		}
	}
	workerAt := map[string]int{}
	for i, w := range v.Workers {
		workerAt[w.ID] = i
	}
	for _, x := range d.Sessions {
		ref := refs[x.WorkerID]
		if ref == "" {
			ref = x.Worker
			if ref == "" {
				ref = x.WorkerID
			}
		}
		x.ShowOwner = all
		x.Path = SessionPath(ref, x.Name)
		x.WorkerURL = WorkerPath(x.WorkerID, all)
		x.StateLabel = stateLabel(x.State)
		if x.State == ops.PaneStateDialog {
			v.WaitingSessions++
			if len(v.Attention) < maxAttention {
				v.Attention = append(v.Attention, AttentionItem{Text: "Session " + x.Name + " on " + x.Worker + " is waiting for an answer.", Link: x.Path, LinkText: "Open session",
					WorkerID: x.WorkerID, Session: x.Name, Target: x.Name + " on " + x.Worker})
			}
		}
		if f.matchSession(x) {
			v.Sessions = append(v.Sessions, x)
		}
		if i, ok := workerAt[x.WorkerID]; ok && len(v.Workers[i].Sessions) < maxWorkerSessions {
			v.Workers[i].Sessions = append(v.Workers[i].Sessions, x)
		}
	}
	v.WorkersText = countText(len(v.Workers), v.TotalWorkers, "worker", "workers")
	v.SessionsText = countText(len(v.Sessions), v.TotalSessions, "session", "sessions")
	return v
}

// discloseSessions makes every Workers row offer its sessions (the Workers page).
func (v *DashboardView) discloseSessions() {
	for i := range v.Workers {
		v.Workers[i].Disclose = true
	}
}

// countText says "3 workers" or, for a filtered list, "2 of 7 workers".
func countText(shown, total int, one, many string) string {
	noun := many
	if shown == 1 && shown == total {
		noun = one
	}
	if shown == total {
		return fmt.Sprintf("%d %s", shown, noun)
	}
	if total == 1 {
		return fmt.Sprintf("%d of 1 %s", shown, one)
	}
	return fmt.Sprintf("%d of %d %s", shown, total, many)
}

func newDashboardPage(d Dashboard, all, canFilter bool) DashboardPage {
	p := DashboardPage{
		DashboardView: newDashboardView(d, all),
		Scope:         scopeMine,
		CanFilter:     canFilter,
		MineURL:       "/",
		AllURL:        "/?scope=all",
		LiveURL:       pathLiveDashboard,
	}
	if all {
		p.Scope = scopeAll
		p.LiveURL = pathLiveDashboard + "?scope=all"
	}
	return p
}

func choices(values []string, current string, label func(string) string) []Choice {
	var out []Choice
	for _, v := range values {
		out = append(out, Choice{Value: v, Label: label(v), Selected: v == current})
	}
	return out
}

func asIs(s string) string { return s }

func distinct(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range list {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	slices.Sort(out)
	return out
}

// listKind selects which list a [ListPage] shows.
type listKind int

const (
	listWorkers listKind = iota
	listSessions
)

func newListPage(d Dashboard, all, canFilter bool, f ListFilter, kind listKind) ListPage {
	page, live := pathWorkers, pathLiveWorkers
	if kind == listSessions {
		page, live = pathSessions, pathLiveSessions
	}
	p := ListPage{DashboardView: newFilteredView(d, all, f), Scope: scopeMine, CanFilter: canFilter, Path: page}
	if kind == listWorkers {
		p.discloseSessions()
	}
	q := f.Values()
	withScope := func(all bool) string {
		v := url.Values{}
		for k, x := range q {
			v[k] = x
		}
		if all {
			v.Set("scope", scopeAll)
		}
		if len(v) == 0 {
			return page
		}
		return page + "?" + v.Encode()
	}
	p.MineURL, p.AllURL = withScope(false), withScope(true)
	p.ClearURL = page
	p.LiveURL = live
	if all {
		p.Scope = scopeAll
		p.ClearURL = page + "?scope=all"
		q.Set("scope", scopeAll)
	}
	if len(q) > 0 {
		p.LiveURL = live + "?" + q.Encode()
	}
	// The choices come from the whole scope, so that narrowing one filter never
	// removes the way back.
	whole := newFilteredView(d, all, ListFilter{})
	var owners, kinds []string
	for _, w := range whole.Workers {
		owners = append(owners, w.Owner)
		if kind == listWorkers {
			kinds = append(kinds, w.Kind)
		}
	}
	for _, x := range whole.Sessions {
		if kind == listSessions {
			owners = append(owners, x.Owner)
			kinds = append(kinds, x.Kind)
		}
	}
	for _, w := range whole.Workers {
		if f.Worker != "" && w.ID == f.Worker {
			p.WorkerName = w.Name
		}
	}
	if kind == listWorkers {
		p.Statuses = choices([]string{"online", "offline"}, f.Status, func(s string) string { return strings.ToUpper(s[:1]) + s[1:] })
	} else {
		p.Statuses = choices([]string{ops.PaneStateStable, ops.PaneStateBusy, ops.PaneStateDialog, ops.PaneStateUnknown, "worker-offline"}, f.Status, func(s string) string {
			if s == "worker-offline" {
				return "Worker offline"
			}
			return stateLabel(s)
		})
	}
	p.DashboardView.ClearURL = p.ClearURL
	p.Kinds = choices(distinct(kinds), f.Kind, asIs)
	if all {
		p.Owners = choices(distinct(owners), f.Owner, asIs)
	}
	return p
}

// fragmentTemplates renders the "dashboard" fragment for the live stream; the
// page itself includes the same template through the layout.
var fragmentTemplates = template.Must(template.ParseFS(EmbeddedTemplates(), "_icons.html", "_session_row.html", "_lists.html", "sessions.html"))

func renderDashboard(v DashboardView) (string, error) { return renderListFragment("dashboard", v) }

func renderListFragment(name string, v DashboardView) (string, error) {
	var buf bytes.Buffer
	if err := fragmentTemplates.ExecuteTemplate(&buf, name, v); err != nil {
		return "", fmt.Errorf("web: render the %s fragment: %w", name, err)
	}
	return buf.String(), nil
}

// ---- the page ----

func dashboardData(get func() LiveSource) func(*http.Request) (any, error) {
	return func(r *http.Request) (any, error) {
		src := get()
		if src == nil {
			return nil, &StatusError{Code: http.StatusServiceUnavailable, Message: "The dashboard is not connected to the Hub yet."}
		}
		all, canFilter := dashboardScope(r)
		d, err := src.Snapshot(r, all)
		if err != nil {
			return nil, pageError(err)
		}
		page := newDashboardPage(d, all, canFilter)
		page.Context = principalContext(r)
		return page, nil
	}
}

func registeredLiveHandler(get func() LiveSource, log *slog.Logger) *liveHandler {
	h := newLiveHandler(get, defaultLiveDebounce, defaultLiveHeartbeat, log)
	h.global = true
	return h
}

// listLiveHandler is the stream of a filtered list page: the filter is the
// query of the stream's own request, so a reconnect keeps it.
func listLiveHandler(get func() LiveSource, log *slog.Logger, kind listKind) *liveHandler {
	h := registeredLiveHandler(get, log)
	name := "workers_results"
	if kind == listSessions {
		name = "sessions_results"
	}
	h.render = func(d Dashboard, all bool, r *http.Request) (string, error) {
		v := newFilteredView(d, all, parseListFilter(r))
		v.ClearURL = pathWorkers
		if kind == listSessions {
			v.ClearURL = pathSessions
		} else {
			v.discloseSessions()
		}
		if all {
			v.ClearURL += "?scope=all"
		}
		return renderListFragment(name, v)
	}
	return h
}

func listData(get func() LiveSource, kind listKind) func(*http.Request) (any, error) {
	return func(r *http.Request) (any, error) {
		src := get()
		if src == nil {
			return nil, &StatusError{Code: http.StatusServiceUnavailable, Message: "This page is not connected to the Hub yet."}
		}
		all, canFilter := dashboardScope(r)
		d, err := src.Snapshot(r, all)
		if err != nil {
			return nil, pageError(err)
		}
		page := newListPage(d, all, canFilter, parseListFilter(r), kind)
		page.Context = principalContext(r)
		if kind == listWorkers {
			page.IdentityNote, page.IdentityTone = workersCountdown(r)
		}
		return page, nil
	}
}

// registerDashboard adds the control center, the Workers and sessions lists,
// their navigation entries and their live streams to reg. get returns the
// source at request time.
func registerDashboard(reg *Registry, get func() LiveSource, log *slog.Logger) error {
	if err := reg.RegisterPage(Page{Pattern: pathHome, Title: "Control center", Template: "sessions.html", Inspector: true, Data: dashboardData(get)}); err != nil {
		return err
	}
	if err := reg.RegisterRoute(Route{Pattern: "GET " + pathLiveDashboard, Handler: registeredLiveHandler(get, log)}); err != nil {
		return err
	}
	if err := reg.RegisterPage(Page{Pattern: pathWorkers, Title: "Workers", Template: "workers.html", Inspector: true, Data: listData(get, listWorkers)}); err != nil {
		return err
	}
	if err := reg.RegisterRoute(Route{Pattern: "GET " + pathLiveWorkers, Handler: listLiveHandler(get, log, listWorkers)}); err != nil {
		return err
	}
	if err := reg.RegisterPage(Page{Pattern: pathSessions, Title: "Sessions", Template: "sessionlist.html", Inspector: true, Data: listData(get, listSessions)}); err != nil {
		return err
	}
	if err := reg.RegisterRoute(Route{Pattern: "GET " + pathLiveSessions, Handler: listLiveHandler(get, log, listSessions)}); err != nil {
		return err
	}
	if err := registerWorkerPage(reg, get, log); err != nil {
		return err
	}
	for _, n := range []NavItem{
		{Label: "Control center", Path: "/", Order: 100, Icon: "i-home"},
		{Label: "Workers", Path: pathWorkers, Order: 110, Icon: "i-workers"},
		{Label: "Sessions", Path: pathSessions, Order: 120, Icon: "i-sessions"},
	} {
		if err := reg.RegisterNav(n); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	must(registerDashboard(DefaultRegistry, currentLiveSource, nil))
}

// ---- the live stream ----

// LiveTimers sets the timers of the Hub's dashboard stream. A zero field
// keeps the default. It exists for composed-Hub tests, which cannot wait for
// the production periods.
type LiveTimers struct {
	Debounce, Heartbeat, Reconcile, WriteTimeout, BodyTimeout time.Duration
}

var liveTimers atomic.Pointer[LiveTimers]

// SetLiveTimers overrides the timers of the registered dashboard stream and
// returns the function that removes the override.
func SetLiveTimers(t LiveTimers) (restore func()) {
	liveTimers.Store(&t)
	return func() { liveTimers.Store(nil) }
}

// timers returns the effective timers of one request: the handler's own, with
// the non-zero fields of [SetLiveTimers] on top for the registered handler.
func (h *liveHandler) timers() LiveTimers {
	t := LiveTimers{Debounce: h.debounce, Heartbeat: h.heartbeat, Reconcile: h.reconcile,
		WriteTimeout: h.writeTimeout, BodyTimeout: h.bodyTimeout}
	if o := liveTimers.Load(); o != nil && h.global {
		for _, f := range []struct{ dst, src *time.Duration }{
			{&t.Debounce, &o.Debounce}, {&t.Heartbeat, &o.Heartbeat}, {&t.Reconcile, &o.Reconcile},
			{&t.WriteTimeout, &o.WriteTimeout}, {&t.BodyTimeout, &o.BodyTimeout},
		} {
			if *f.src > 0 {
				*f.dst = *f.src
			}
		}
	}
	return t
}

// maxLiveBodyBytes bounds a request body that a client announces on the
// stream's GET and that the server reads and discards.
const maxLiveBodyBytes = 1 << 20

type liveHandler struct {
	get func() LiveSource
	// debounce is the batching window: the time between the first change of a
	// burst and the render. It does not restart on later changes, so a feed
	// that never pauses (a busy session) still renders every window.
	debounce time.Duration
	// heartbeat is the keep-alive comment interval.
	heartbeat time.Duration
	// reconcile is the period of a full snapshot without any event. Changes
	// the event bus does not carry (a Worker renamed, removed or reassigned
	// while offline) show up at the latest then.
	reconcile time.Duration
	// writeTimeout bounds one write to the browser.
	writeTimeout time.Duration
	// bodyTimeout bounds the time a client may take to send a request body it
	// announced on the GET.
	bodyTimeout time.Duration
	// global lets [SetLiveTimers] override the timers (the registered handler).
	global bool
	// render builds the fragment of one snapshot; nil renders the control
	// center. The Workers and sessions pages render their filtered lists from
	// the query of the stream's own request.
	render func(d Dashboard, all bool, r *http.Request) (string, error)
	log    *slog.Logger
}

func newLiveHandler(get func() LiveSource, debounce, heartbeat time.Duration, log *slog.Logger) *liveHandler {
	if log == nil {
		log = slog.Default()
	}
	return &liveHandler{
		get: get, debounce: debounce, heartbeat: heartbeat, log: log,
		reconcile: defaultLiveReconcile, writeTimeout: defaultLiveWriteTimeout, bodyTimeout: defaultLiveBodyTimeout,
	}
}

// absorbRequestBody reads and discards a request body that the client
// announced, under a read deadline on the connection of the ORIGINAL request.
// net/http reads an unread body before it sends the first response bytes, so a
// client that announces a body and withholds it would otherwise hold the
// stream's start (and the feed subscribed before it) for ever; the Hub's server
// timeouts do not cover bodies, and the in-process API request that the source
// makes has no such body to bound. On failure the connection is marked for
// closing and the deadline stays, so the rest of the body is not waited for
// either.
func absorbRequestBody(w http.ResponseWriter, r *http.Request, timeout time.Duration) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(timeout))
	n, err := io.Copy(io.Discard, io.LimitReader(r.Body, maxLiveBodyBytes+1))
	if err != nil || n > maxLiveBodyBytes {
		w.Header().Set("Connection", "close")
		return errors.New("web: the request body could not be read")
	}
	_ = rc.SetReadDeadline(time.Time{})
	return nil
}

// ServeHTTP streams the "dashboard" event: once on connect (a change between
// the page render and the connection is not lost), then after every change,
// and on a slow timer for what the feed does not carry.
func (h *liveHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	src := h.get()
	if src == nil {
		http.Error(w, "the dashboard is not connected to the Hub yet", http.StatusServiceUnavailable)
		return
	}
	tm := h.timers()
	// Before anything is subscribed or written.
	if err := absorbRequestBody(w, r, tm.BodyTimeout); err != nil {
		http.Error(w, "the request body could not be read", http.StatusBadRequest)
		return
	}
	all, _ := dashboardScope(r)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Subscribe before the first snapshot: a change in between is rendered
	// twice at worst, never lost.
	changes, err := src.Changes(ctx, r, all)
	if err != nil {
		var oe *OpError
		if errors.As(err, &oe) && oe.Status >= 400 && oe.Status < 600 {
			http.Error(w, oe.Message, oe.Status)
			return
		}
		h.log.Error("dashboard feed failed", "error", err)
		http.Error(w, "the live feed is not available", http.StatusBadGateway)
		return
	}
	out, err := NewSSE(w)
	if err != nil {
		h.log.Error("dashboard stream failed", "error", err)
		return
	}
	out.SetWriteTimeout(tm.WriteTimeout)
	defer out.Finish()

	// pump turns the feed into at most one pending signal. While the loop
	// below is blocked in a write it cannot see that the feed has ended (access
	// lost, logout, shutdown), so the pump interrupts that write itself.
	sig := make(chan struct{}, 1)
	ended := make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-changes:
				if !ok {
					close(ended)
					out.Interrupt()
					return
				}
				select {
				case sig <- struct{}{}:
				default: // one pending signal is enough
				}
			}
		}
	}()

	if out.Retry(liveRetry) != nil {
		return
	}
	last := ""
	push := func() bool {
		d, err := src.Snapshot(r.WithContext(ctx), all)
		if err != nil {
			if ctx.Err() == nil {
				h.log.Warn("dashboard snapshot failed", "error", err)
			}
			return false
		}
		var html string
		if h.render != nil {
			html, err = h.render(d, all, r)
		} else {
			html, err = renderDashboard(newDashboardView(d, all))
		}
		if err != nil {
			h.log.Error("dashboard render failed", "error", err)
			return false
		}
		if html == last {
			return true
		}
		last = html
		return out.Send(Message{Event: "dashboard", Data: html}) == nil
	}
	if !push() {
		return
	}
	beat := time.NewTicker(tm.Heartbeat)
	defer beat.Stop()
	sweep := time.NewTicker(tm.Reconcile)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ended:
			return
		case <-beat.C:
			if out.Comment("keep-alive") != nil {
				return
			}
		case <-sweep.C:
			if !push() {
				return
			}
		case <-sig:
			if !window(ctx, ended, tm.Debounce) {
				return
			}
			// Whatever arrived during the window is in the snapshot below.
			select {
			case <-sig:
			default:
			}
			if !push() {
				return
			}
		}
	}
}

// window waits one batching window. It is fixed: further changes do not extend
// it, so a feed that never pauses cannot hold the render (or the heartbeat)
// back. It reports false when the feed ended or ctx is done.
func window(ctx context.Context, ended <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-ended:
		return false
	case <-t.C:
		return true
	}
}

// ---- the in-process source ----

// handlerSource implements [LiveSource] on the REST API of this Hub.
type handlerSource struct {
	c *HandlerCaller
}

// NewHandlerSource returns a [LiveSource] that reads the Hub's REST API through
// the in-process caller c, as the signed-in user of each request.
func NewHandlerSource(c *HandlerCaller) LiveSource { return &handlerSource{c: c} }

type sessionsBody struct {
	Sessions []struct {
		WorkerID string `json:"worker_id"`
		Worker   string `json:"worker"`
		Owner    string `json:"owner"`
		Session  string `json:"session"`
		Kind     string `json:"kind"`
		State    string `json:"state"`
		Task     string `json:"task"`
		Online   bool   `json:"online"`
	} `json:"sessions"`
}

// Snapshot implements [LiveSource].
func (s *handlerSource) Snapshot(r *http.Request, all bool) (Dashboard, error) {
	var d Dashboard
	var wl ops.WorkersListResult
	if err := s.c.CallOp(r, ops.OpWorkersList, ops.WorkersListArgs{All: all}, &wl); err != nil {
		if !notServed(err) {
			return Dashboard{}, err
		}
		d.Notice = noticeNotServed
	}
	rec := &capture{header: http.Header{}, limit: maxOpResponseBytes}
	req, err := s.c.internalGet(r.Context(), r, apiSessionsPath+allQuery(all))
	if err != nil {
		return Dashboard{}, err
	}
	s.c.h.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	if rec.over {
		return Dashboard{}, fmt.Errorf("web: the session list is larger than %d bytes", maxOpResponseBytes)
	}
	var sb sessionsBody
	switch {
	case rec.status/100 == 2:
		if err := json.Unmarshal(rec.body.Bytes(), &sb); err != nil {
			return Dashboard{}, fmt.Errorf("web: decode the session list: %w", err)
		}
	case notServed(decodeOpError(rec.status, rec.body.Bytes())):
		d.Notice = noticeNotServed
	default:
		return Dashboard{}, decodeOpError(rec.status, rec.body.Bytes())
	}
	for _, w := range wl.Workers {
		d.Workers = append(d.Workers, WorkerRow{
			ID: w.ID, Name: w.Name, Owner: w.Owner, Kind: w.Kind, Online: w.Online, Version: w.ProtoVersion,
			Pending: w.Pending, Fingerprint: w.Fingerprint, EnrollAddr: w.EnrollAddr, NeverConnected: w.NeverConnected,
		})
	}
	for _, x := range sb.Sessions {
		d.Sessions = append(d.Sessions, SessionRow{
			WorkerID: x.WorkerID, Worker: x.Worker, Owner: x.Owner, Name: x.Session,
			Kind: x.Kind, State: x.State, Task: x.Task, Online: x.Online,
		})
	}
	return d, nil
}

// noticeNotServed is shown when the Hub has no Worker list or session list
// (a Hub assembled without those modules): an empty dashboard with a reason
// is better than an error page for a home page.
const noticeNotServed = "This Hub does not serve the live Worker and session lists."

// notServed reports whether err says that the Hub has no such endpoint: 404
// for an unmounted route, 501 for an operation that is not available.
func notServed(err error) bool {
	var oe *OpError
	return errors.As(err, &oe) && (oe.Status == http.StatusNotFound || oe.Status == http.StatusNotImplemented)
}

func allQuery(all bool) string {
	if all {
		return "?all=true"
	}
	return ""
}

// internalGet builds an in-process GET that carries the credentials and the
// client address headers of the incoming request r (see [HandlerCaller.CallOp]).
func (c *HandlerCaller) internalGet(ctx context.Context, r *http.Request, target string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("web: build the request of %s: %w", target, err)
	}
	req.Header.Set("Accept", "application/json")
	if sess, ok := session.FromContext(r.Context()); ok {
		req.Header.Set(session.CSRFHeader, sess.CSRFToken)
	}
	if v := r.Header.Get("Cookie"); v != "" {
		req.Header.Set("Cookie", v)
	}
	for _, h := range forwardedHeaders {
		for _, v := range r.Header.Values(h) {
			req.Header.Add(h, v)
		}
	}
	req.Host, req.RemoteAddr, req.TLS = r.Host, r.RemoteAddr, r.TLS
	return req, nil
}

// Changes implements [LiveSource]: it follows GET /api/v1/events in process and
// turns every event into a change signal.
func (s *handlerSource) Changes(ctx context.Context, r *http.Request, all bool) (<-chan struct{}, error) {
	ctx, cancel := context.WithCancel(ctx)
	req, err := s.c.internalGet(ctx, r, apiEventsPath+allQuery(all))
	if err != nil {
		cancel()
		return nil, err
	}
	pr, pw := io.Pipe()
	sw := &streamWriter{header: http.Header{}, pw: pw, ready: make(chan struct{})}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				_ = pw.CloseWithError(fmt.Errorf("web: the event feed panicked: %v", p))
			}
			sw.WriteHeader(http.StatusOK)
			_ = pw.Close()
		}()
		s.c.h.ServeHTTP(sw, req)
	}()
	select {
	case <-sw.ready:
	case <-ctx.Done():
		_ = pr.CloseWithError(ctx.Err())
		cancel()
		return nil, ctx.Err()
	}
	if sw.status != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(pr, 64<<10))
		_ = pr.CloseWithError(io.EOF)
		cancel()
		return nil, decodeOpError(sw.status, body)
	}
	out := make(chan struct{}, 1)
	go func() {
		defer close(out)
		defer cancel()
		defer func() { _ = pr.Close() }()
		go func() { // unblock the read when the caller is done
			<-ctx.Done()
			_ = pr.CloseWithError(ctx.Err())
		}()
		followEvents(pr, func() {
			select {
			case out <- struct{}{}:
			default: // one pending signal is enough
			}
		})
	}()
	return out, nil
}

// streamWriter is the response writer of an in-process stream: the body goes
// into a pipe that the reader follows while the handler is still running.
type streamWriter struct {
	header http.Header
	pw     *io.PipeWriter
	once   sync.Once
	status int
	ready  chan struct{}
}

func (w *streamWriter) Header() http.Header { return w.header }

func (w *streamWriter) WriteHeader(code int) {
	w.once.Do(func() {
		w.status = code
		close(w.ready)
	})
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pw.Write(p)
}

// Flush implements http.Flusher: the pipe has no buffer.
func (w *streamWriter) Flush() { w.WriteHeader(http.StatusOK) }

// followEvents reads an SSE stream and calls changed for every event that
// carries a name, except the final "reset" and "access_lost", which end it.
// Only the short "event:" lines matter; long data lines are skipped.
func followEvents(rd io.Reader, changed func()) {
	br := bufio.NewReaderSize(rd, 4096)
	name := ""
	for {
		line, long, err := readLine(br)
		if err != nil {
			return
		}
		switch {
		case long:
		case line == "":
			ev := name
			name = ""
			switch ev {
			case "":
			case "reset", "access_lost":
				return
			default:
				changed()
			}
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
	}
}

// readLine returns one line without its terminator. A line longer than the
// buffer is consumed to its end and reported as long with no content.
func readLine(br *bufio.Reader) (line string, long bool, err error) {
	var sb []byte
	for {
		part, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			long = true
			continue
		}
		if err != nil {
			return "", false, err
		}
		if long {
			return "", true, nil
		}
		sb = append(sb, part...)
		return strings.TrimRight(string(sb), "\r\n"), false, nil
	}
}
