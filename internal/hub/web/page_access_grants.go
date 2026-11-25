package web

// Access > Access (session grants): which session may reach which session of
// another Worker, and at which level. The list comes from grant_list; every
// row action is one small form whose answer is the row as the Hub has it now
// (never optimistic), with a status line inside the row. Adding a grant is a
// staged form in a dialog. The operations decide what a user sees and may
// change; this page only hides what cannot work.
// Design: docs/minion-session-grants/DESIGN.md 11.3 and 11.5.

import (
	"errors"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const pathAccessGrants = "/access/grants"

// grantsChangedEvent asks the list to reload itself from the Hub.
const grantsChangedEvent = "grants-changed"

const grantConflictText = "Changed by someone else. This is the current state."

// GrantsPage is what access_grants.html sees as .Data.
type GrantsPage struct {
	Rows  []GrantRow
	Admin bool
	// Sources are the Workers a grant may start from (the caller's own for a
	// non-administrator); Targets are every Worker the caller sees.
	Sources []GrantWorkerOption
	Targets []GrantWorkerOption
}

// GrantWorkerOption is one Worker of the Add dialog with its live sessions.
type GrantWorkerOption struct {
	ID, Name, Owner string
	Sessions        []string
}

// GrantRow is one grant of the table.
type GrantRow struct {
	ops.Grant
	// Admin is true when the viewer is an administrator: Approve is offered.
	Admin bool
	// Gone is true when the grant is no longer listed (after a revoke).
	Gone  bool
	Saved string
	Error string
}

func (g GrantRow) SourceWorker() string {
	if g.SourceWorkerName != "" {
		return g.SourceWorkerName
	}
	return g.SourceWorkerID
}

func (g GrantRow) TargetWorker() string {
	if g.TargetWorkerName != "" {
		return g.TargetWorkerName
	}
	return g.TargetWorkerID
}

// StateKey is the state of the grant: the Hub's word, or the same derived
// from the row when an older Hub does not send it.
func (g GrantRow) StateKey() string {
	switch {
	case g.State != "":
		return g.State
	case g.Suspended:
		return ops.GrantStateSuspended
	case g.ExpiresAt != nil && !g.ExpiresAt.IsZero() && g.ExpiresAt.Time().Before(time.Now()):
		return ops.GrantStateExpired
	}
	return ops.GrantStateActive
}

func (g GrantRow) StateLabel() string {
	switch g.StateKey() {
	case ops.GrantStateSuspended:
		return "Suspended: needs an administrator"
	case ops.GrantStateExpired:
		return "Expired"
	}
	return "In effect"
}

func (g GrantRow) StateTone() string {
	switch g.StateKey() {
	case ops.GrantStateSuspended:
		return "danger"
	case ops.GrantStateExpired:
		return "warning"
	}
	return "positive"
}

func (g GrantRow) IsSuspended() bool { return g.StateKey() == ops.GrantStateSuspended }

// Off is true for a grant that does not work now (suspended or expired).
func (g GrantRow) Off() bool { return g.StateKey() != ops.GrantStateActive }

// Between is true for a grant between Workers of two different owners.
func (g GrantRow) Between() bool {
	return g.SourceOwner != "" && g.TargetOwner != "" && g.SourceOwner != g.TargetOwner
}

func (g GrantRow) Verified() bool { return g.SourceProof == ops.GrantProofVerified }

func (g GrantRow) LevelLabel() string { return grantLevelLabel(g.Level) }

func (g GrantRow) AuthorityLabel() string {
	if g.Authority == ops.GrantAuthorityAdmin {
		return "administrator"
	}
	return "owner"
}

func (g GrantRow) UntilText() string {
	if g.ExpiresAt == nil || g.ExpiresAt.IsZero() {
		return "no end"
	}
	return g.ExpiresAt.Time().Format("2 Jan 2006")
}

// ExpiresAtValue is the end of the grant in the form the operations read, so
// that a change of the level keeps it.
func (g GrantRow) ExpiresAtValue() string {
	if g.ExpiresAt == nil || g.ExpiresAt.IsZero() {
		return ""
	}
	return g.ExpiresAt.Time().Format("2006-01-02T15:04:05.000Z")
}

// FilterText is what the filter box searches.
func (g GrantRow) FilterText() string {
	return strings.Join([]string{g.SourceSession, g.SourceWorker(), g.SourceOwner, g.TargetSession, g.TargetWorker(), g.TargetOwner, g.Level, g.GrantedBy}, " ")
}

func grantLevelLabel(level string) string {
	switch level {
	case ops.GrantLevelView:
		return "View"
	case ops.GrantLevelMessage:
		return "Message"
	case ops.GrantLevelControl:
		return "Control"
	}
	return level
}

var grantTemplates = sync.OnceValue(func() *template.Template {
	return template.Must(template.ParseFS(templateFiles, "templates/access_grants.html", "templates/_grant_row.html"))
})

func grantRows(r *http.Request, c OpCaller, admin bool) ([]GrantRow, error) {
	var res ops.GrantListResult
	if err := c.CallOp(r, ops.OpGrantList, ops.GrantListArgs{}, &res); err != nil {
		return nil, err
	}
	rows := make([]GrantRow, 0, len(res.Grants))
	for _, g := range res.Grants {
		rows = append(rows, GrantRow{Grant: g, Admin: admin})
	}
	return rows, nil
}

// grantWorkers are the Workers and live sessions of the Add dialog, from the
// snapshot the session list uses. A non-administrator may start a grant only
// from a Worker of their own.
func grantWorkers(r *http.Request, u pageUser) (sources, targets []GrantWorkerOption) {
	src := currentLiveSource()
	if src == nil {
		return nil, nil
	}
	d, err := src.Snapshot(r, u.Admin)
	if err != nil {
		return nil, nil
	}
	sessions := map[string][]string{}
	for _, s := range d.Sessions {
		sessions[s.WorkerID] = append(sessions[s.WorkerID], s.Name)
	}
	for _, w := range d.Workers {
		names := sessions[w.ID]
		slices.SortFunc(names, ops.NaturalCompare)
		o := GrantWorkerOption{ID: w.ID, Name: w.Name, Owner: w.Owner, Sessions: names}
		targets = append(targets, o)
		if u.Admin || w.Owner == u.Username {
			sources = append(sources, o)
		}
	}
	return sources, targets
}

func grantsData(r *http.Request, c OpCaller, u pageUser) (any, error) {
	rows, err := grantRows(r, c, u.Admin)
	if err != nil {
		return nil, err
	}
	p := GrantsPage{Rows: rows, Admin: u.Admin}
	p.Sources, p.Targets = grantWorkers(r, u)
	return p, nil
}

func grantFailure(err error) string {
	var oe *OpError
	if errors.As(err, &oe) && oe.Status == http.StatusConflict {
		return grantConflictText
	}
	return failureText(err)
}

// splitSession reads a session choice "<worker id>|<session>" (ids never
// hold "|").
func splitSession(v string) (worker, session string) {
	if i := strings.IndexByte(v, '|'); i > 0 {
		return v[:i], v[i+1:]
	}
	return "", v
}

// handleGrantChange applies one action. A row action answers the row as the
// Hub has it now; "add" answers a status line for the dialog and asks the
// list to reload.
func handleGrantChange(w http.ResponseWriter, r *http.Request, c OpCaller, u pageUser) {
	f := r.PostForm
	action := r.PathValue("action")
	if action == "add" {
		handleGrantAdd(w, r, c)
		return
	}
	id := f.Get("id")
	// An unreadable revision is sent as absent: the operation refuses it.
	rev, _ := strconv.ParseUint(f.Get("if_revision"), 10, 64)
	var (
		callErr error
		saved   = "Saved"
	)
	switch action {
	case "set-level":
		a := ops.GrantSetArgs{
			WriteArgs: newWriteArgs(), ID: id, IfRevision: rev, Level: f.Get("level"),
			SourceWorkerID: f.Get("source_worker_id"), SourceSession: f.Get("source_session"),
			TargetWorkerID: f.Get("target_worker_id"), TargetSession: f.Get("target_session"),
			ExpiresAt: f.Get("expires_at"), Note: f.Get("note"),
		}
		var res ops.GrantSetResult
		callErr = c.CallOp(r, ops.OpGrantSet, a, &res)
	case "revoke":
		var res ops.WriteResult
		callErr = c.CallOp(r, ops.OpGrantRevoke, ops.GrantRevokeArgs{WriteArgs: newWriteArgs(), ID: id, IfRevision: rev}, &res)
		saved = "Revoked"
	case "approve":
		if !u.Admin {
			http.NotFound(w, r)
			return
		}
		var res ops.GrantSetResult
		callErr = c.CallOp(r, ops.OpGrantApprove, ops.GrantApproveArgs{WriteArgs: newWriteArgs(), ID: id, IfRevision: rev}, &res)
		saved = "Approved"
	default:
		http.NotFound(w, r)
		return
	}
	// Answer with the row as the Hub has it now, whatever happened.
	row := GrantRow{Grant: ops.Grant{ID: id}, Admin: u.Admin, Gone: true}
	rows, err := grantRows(r, c, u.Admin)
	if err != nil {
		writeResultError(w, failureText(err))
		return
	}
	for _, g := range rows {
		if g.ID == id {
			row = g
			break
		}
	}
	if callErr != nil {
		row.Error = grantFailure(callErr)
	} else {
		row.Saved = saved
	}
	writeFragmentTemplate(w, grantTemplates(), "grant_row", row)
}

// handleGrantAdd creates one grant from the Add dialog.
func handleGrantAdd(w http.ResponseWriter, r *http.Request, c OpCaller) {
	f := r.PostForm
	a := ops.GrantSetArgs{
		WriteArgs: newWriteArgs(), SourceWorkerID: f.Get("source_worker_id"), TargetWorkerID: f.Get("target_worker_id"),
		Level: f.Get("level"), Note: strings.TrimSpace(f.Get("note")),
	}
	sw, ss := splitSession(f.Get("source_session"))
	tw, ts := splitSession(f.Get("target_session"))
	switch {
	case a.SourceWorkerID == "" || ss == "":
		writeResultError(w, "Choose the source Worker and its session.")
		return
	case a.TargetWorkerID == "":
		writeResultError(w, "Choose the target Worker.")
		return
	case sw != "" && sw != a.SourceWorkerID:
		writeResultError(w, "The source session is not on the chosen source Worker.")
		return
	case tw != "" && tw != a.TargetWorkerID:
		writeResultError(w, "The target session is not on the chosen target Worker.")
		return
	}
	a.SourceSession, a.TargetSession = ss, ts
	switch f.Get("until") {
	case "7", "30":
		days, _ := strconv.Atoi(f.Get("until"))
		a.ExpiresAt = time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour).Format("2006-01-02T15:04:05.000Z")
	}
	var res ops.GrantSetResult
	if err := c.CallOp(r, ops.OpGrantSet, a, &res); err != nil {
		writeResultError(w, grantFailure(err))
		return
	}
	if r.Header.Get("HX-Request") == "" {
		// Without scripts the form is a plain post: back to the list.
		http.Redirect(w, r, pathAccessGrants, http.StatusSeeOther)
		return
	}
	w.Header().Set("HX-Trigger", grantsChangedEvent)
	writeFragment(w, []byte(`<p class="grant-status" role="status">Saved</p>`))
}

func init() {
	RegisterPage(Page{Pattern: pathAccessGrants, Title: "Access", Template: "access_grants.html", Data: userPageData(grantsData)})
	RegisterNav(NavItem{Label: "Access", Path: pathAccessGrants, Order: onboardingNavOrder + 15, Group: NavGroupAccess, Icon: "i-shield"})
	RegisterRoute(Route{Pattern: "POST " + pathAccessGrants + "/{action}", Handler: userRoute(handleGrantChange)})
}
