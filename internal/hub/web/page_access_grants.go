package web

// Access > Access (session grants): which session may reach which session of
// another Worker, and at which level. The list comes from grant_list; every
// row action is one small form whose answer is the row as the Hub has it now
// (never optimistic), with a status line inside the row. Adding a grant is a
// staged form in a dialog. The operations decide what a user sees and may
// change; this page only hides what cannot work.
// Design: docs/minion-session-grants/DESIGN.md 11.3 and 11.5.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
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

// grantUnconfirmedEvent tells the page that the row of one grant may no
// longer show what the Hub has: its controls stay off until the list reloads.
const grantUnconfirmedEvent = "grant-unconfirmed"

// GrantsPage is what access_grants.html sees as .Data.
type GrantsPage struct {
	Rows  []GrantRow
	Admin bool
	// Sources are the Workers a grant may start from (the caller's own for a
	// non-administrator); Targets are every Worker the caller sees.
	Sources []GrantWorkerOption
	Targets []GrantWorkerOption
	// Worker limits the list to the grants that touch one Worker (?worker=);
	// WorkerName names it for the "Showing grants of" line.
	Worker, WorkerName string
	// SourceWorker and SourceSession preselect the source of the Add form
	// (?source=<worker id>|<session>); the form then opens by itself.
	SourceWorker, SourceSession string
}

// ListURL is the address the list reloads itself from: it keeps the Worker
// filter of the page.
func (p GrantsPage) ListURL() string {
	if p.Worker == "" {
		return pathAccessGrants
	}
	return pathAccessGrants + "?worker=" + url.QueryEscape(p.Worker)
}

// OpenAdd is true when the page was asked to open the Add form with a source.
func (p GrantsPage) OpenAdd() bool { return p.SourceWorker != "" && p.SourceSession != "" }

// GrantWorkerOption is one Worker of the Add dialog with its live sessions.
type GrantWorkerOption struct {
	ID, Name, Owner string
	Sessions        []string
}

// GrantRow is one grant of the table.
type GrantRow struct {
	ops.Grant
	// Admin is true when the viewer is an administrator: Approve and the
	// History link are offered.
	Admin bool
	// Viewer and ViewerID name the viewer (the user acted as, for an
	// administrator acting as a user): CanEdit and CanRevoke read them.
	Viewer, ViewerID string
	// DecidedBy is the name of the user in GrantedBy; empty when the viewer
	// cannot know it (the id is shown then).
	DecidedBy string
	// Gone is true when the grant is no longer listed (after a revoke).
	Gone  bool
	Saved string
	Error string
	// Effect is what the access matrix says about the pair of this grant.
	Effect GrantEffect
	// Confirm is the sentence of the second step of a change that widens
	// access; ConfirmLevel is the level it would set.
	Confirm      string
	ConfirmLevel string
}

// GrantEffect is the result of a grant as the access matrix has it: the
// stored state of a grant alone never says that it works.
type GrantEffect struct {
	// Known is false when the matrix could not be read or has no such pair.
	Known bool
	// Level is the level that works now; empty while a layer blocks it.
	Level string
	// BlockedBy is the first failing layer (ops.AccessBlocked...).
	BlockedBy string
	// Reached of Covered target sessions work now (a wildcard grant covers
	// several).
	Reached, Covered int
}

// EffectLabel is the level in effect, with the count for a wildcard grant
// that reaches only some of the sessions it covers.
func (g GrantRow) EffectLabel() string {
	l := grantLevelLabel(g.Effect.Level)
	if g.Effect.Covered > 1 {
		return l + " on " + strconv.Itoa(g.Effect.Reached) + " of " + strconv.Itoa(g.Effect.Covered) + " sessions"
	}
	return l
}

// BlockedText names the blocking layer for people.
func (g GrantRow) BlockedText() string { return accessBlockText(g.Effect.BlockedBy) }

// CanEdit is the one rule of who may change the level of a grant, for the
// row and for the handler alike (the Hub's rule of grant_set): an
// administrator, or the owner of both Workers of a grant of owner authority.
func (g GrantRow) CanEdit() bool {
	if g.Masked() {
		return false
	}
	if g.Admin {
		return true
	}
	return g.Authority == ops.GrantAuthorityOwner && g.Viewer != "" && g.SourceOwner == g.Viewer && g.TargetOwner == g.Viewer
}

// CanRevoke is the one rule of who may revoke a grant: an administrator, the
// owner of either Worker, and the person who decided the grant (the Hub lets
// them withdraw it).
func (g GrantRow) CanRevoke() bool {
	if g.Admin {
		return true
	}
	if g.Viewer != "" && (g.SourceOwner == g.Viewer || g.TargetOwner == g.Viewer) {
		return true
	}
	return g.ViewerID != "" && g.GrantedBy == g.ViewerID
}

// Masked is true when the grant names a session the viewer may not see: the
// row shows the name as the Hub gives it and offers nothing but Revoke.
func (g GrantRow) Masked() bool { return g.SourceMasked || g.TargetMasked }

// TargetText is the target of the grant in words.
func (g GrantRow) TargetText() string {
	if g.TargetSession == "" {
		return "all shared sessions of " + g.TargetWorker()
	}
	return g.TargetSession
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

// StateLabel is the stored state of the grant. "Active" is not "works now":
// the column "In effect" says that, from the access matrix.
func (g GrantRow) StateLabel() string {
	switch g.StateKey() {
	case ops.GrantStateSuspended:
		return "Suspended"
	case ops.GrantStateExpired:
		return "Expired"
	}
	return "Active"
}

func (g GrantRow) StateTone() string {
	switch g.StateKey() {
	case ops.GrantStateSuspended:
		return "danger"
	case ops.GrantStateExpired:
		return "warning"
	}
	return "info"
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
	by := g.DecidedBy
	if by == "" {
		by = g.GrantedBy
	}
	return strings.Join([]string{g.SourceSession, g.SourceWorker(), g.SourceOwner, g.TargetSession, g.TargetWorker(), g.TargetOwner, g.Level, by}, " ")
}

// HistoryURL is the audit log of this grant (administrators only).
func (g GrantRow) HistoryURL() string { return pathAdminAudit + "?grant=" + url.QueryEscape(g.ID) }

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

// grantEffect looks a grant up in the access matrix. m is nil when the matrix
// could not be read. A suspended or expired grant is blocked whatever the
// matrix says. Only the cells the matrix credits to this grant (Via is its id)
// count: a cell of the same endpoints reached through another grant or a
// master says nothing about this one. An active grant without such a cell is
// "unknown".
func grantEffect(g GrantRow, m *ops.AccessMatrixResult) GrantEffect {
	switch g.StateKey() {
	case ops.GrantStateSuspended:
		return GrantEffect{Known: true, BlockedBy: ops.AccessBlockedSuspended}
	case ops.GrantStateExpired:
		return GrantEffect{Known: true, BlockedBy: ops.AccessBlockedExpired}
	}
	var e GrantEffect
	if m != nil && g.ID != "" {
		for _, c := range m.Cells {
			if c.Via != g.ID || c.Source < 0 || c.Source >= len(m.Sources) || c.Target < 0 || c.Target >= len(m.Targets) {
				continue
			}
			e.Known = true
			e.Covered++
			if c.InEffect != "" {
				e.Reached++
				if ra, _ := ops.GrantLevelRank(c.InEffect); e.Level == "" || ra > grantRank(e.Level) {
					e.Level = c.InEffect
				}
			} else if e.BlockedBy == "" {
				e.BlockedBy = c.BlockedBy
			}
		}
	}
	if e.Level != "" {
		e.BlockedBy = ""
		return e
	}
	if !e.Known || e.BlockedBy == "" {
		// Nothing says that it works, and nothing names a blocking layer.
		return GrantEffect{}
	}
	return e
}

func grantRank(level string) int {
	r, _ := ops.GrantLevelRank(level)
	return r
}

// grantUserNames maps the user ids of granted_by to names: the viewer's own,
// and for an administrator every user of users_list. A user who is no
// administrator cannot list users, so another decider keeps the id.
func grantUserNames(r *http.Request, c OpCaller, u pageUser, grants []ops.Grant) map[string]string {
	names := map[string]string{}
	if u.ID != "" && u.Username != "" {
		names[u.ID] = u.Username
	}
	if !u.Admin {
		return names
	}
	for _, g := range grants {
		if _, ok := names[g.GrantedBy]; g.GrantedBy != "" && !ok {
			var res ops.UsersListResult
			if err := c.CallOp(r, ops.OpUsersList, ops.UsersListArgs{}, &res); err == nil {
				for _, x := range res.Users {
					names[x.ID] = x.Username
				}
			}
			break
		}
	}
	return names
}

// grantRows lists the grants as rows; workerID limits them to one Worker.
func grantRows(r *http.Request, c OpCaller, u pageUser, workerID string) ([]GrantRow, error) {
	admin := u.Admin
	var res ops.GrantListResult
	if err := c.CallOp(r, ops.OpGrantList, ops.GrantListArgs{WorkerID: workerID}, &res); err != nil {
		return nil, err
	}
	// The matrix is the read model of "works now". Without it the rows say
	// "unknown" rather than claim an effect from the stored state.
	var matrix *ops.AccessMatrixResult
	if len(res.Grants) > 0 {
		var m ops.AccessMatrixResult
		if err := c.CallOp(r, ops.OpAccessMatrix, ops.AccessMatrixArgs{}, &m); err == nil {
			matrix = &m
		}
	}
	names := grantUserNames(r, c, u, res.Grants)
	rows := make([]GrantRow, 0, len(res.Grants))
	for _, g := range res.Grants {
		row := GrantRow{Grant: g, Admin: admin, Viewer: u.Username, ViewerID: u.ID, DecidedBy: names[g.GrantedBy]}
		row.Effect = grantEffect(row, matrix)
		rows = append(rows, row)
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
	p := GrantsPage{Admin: u.Admin}
	p.Sources, p.Targets = grantWorkers(r, u)
	q := r.URL.Query()
	// ?worker=<id>: a Worker the page knows, or a well-formed id (the
	// operation decides what it lists); anything else shows every grant.
	if w := strings.TrimSpace(q.Get("worker")); w != "" {
		if o, ok := findGrantWorker(p.Targets, w); ok {
			p.Worker, p.WorkerName = w, o.Name
		} else if ops.ValidUUIDv7(w) {
			p.Worker = w
		}
	}
	rows, err := grantRows(r, c, u, p.Worker)
	if err != nil {
		return nil, err
	}
	p.Rows = rows
	if p.Worker != "" && p.WorkerName == "" {
		p.WorkerName = p.Worker
		for _, g := range rows {
			switch {
			case g.SourceWorkerID == p.Worker && g.SourceWorkerName != "":
				p.WorkerName = g.SourceWorkerName
			case g.TargetWorkerID == p.Worker && g.TargetWorkerName != "":
				p.WorkerName = g.TargetWorkerName
			default:
				continue
			}
			break
		}
	}
	// ?source=<worker id>|<session>: only a live session the viewer may
	// start a grant from is preselected.
	if sw, ss := splitSession(q.Get("source")); sw != "" && ss != "" {
		if o, ok := findGrantWorker(p.Sources, sw); ok && slices.Contains(o.Sessions, ss) {
			p.SourceWorker, p.SourceSession = sw, ss
		}
	}
	return p, nil
}

func findGrantWorker(list []GrantWorkerOption, id string) (GrantWorkerOption, bool) {
	for _, o := range list {
		if o.ID == id {
			return o, true
		}
	}
	return GrantWorkerOption{}, false
}

// grantFailure is the sentence for a refused change. Only a revision conflict
// of a row is "changed by someone else"; every other refusal (a duplicate, a
// Worker waiting for approval, a suspended grant) keeps the words of the
// operation. The operations answer all of them with the code "conflict", so
// the revision conflict is told apart by its field or its message.
func grantFailure(err error, row bool) string {
	var oe *OpError
	if row && errors.As(err, &oe) && (oe.Status == http.StatusConflict || oe.Code == "conflict") &&
		(oe.Field == "if_revision" || strings.Contains(oe.Message, "changed meanwhile") || strings.Contains(oe.Message, "revision")) {
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

// grantWidens reports whether a grant needs the explicit second step of
// DESIGN.md 11.3: Control, a wildcard target, or two owners (also when an
// owner is not known). View and Message for one session of one owner need none.
func grantWidens(level, targetSession, sourceOwner, targetOwner string) bool {
	return level == ops.GrantLevelControl || targetSession == "" || sourceOwner == "" || targetOwner == "" || sourceOwner != targetOwner
}

// grantSentence names source, target and level of a change in one sentence.
func grantSentence(sourceSession, sourceWorker, sourceOwner, targetSession, targetWorker, targetOwner, level, until string) string {
	who := func(worker, owner string) string {
		if owner == "" {
			return worker
		}
		return worker + " (" + owner + ")"
	}
	can := "see"
	switch level {
	case ops.GrantLevelMessage:
		can = "see and send prompts to"
	case ops.GrantLevelControl:
		can = "see, send prompts to and type into"
	}
	target := "the session " + targetSession + " on " + who(targetWorker, targetOwner)
	if targetSession == "" {
		target = "every session that " + who(targetWorker, targetOwner) + " shares, now and later"
	}
	return sourceSession + " on " + who(sourceWorker, sourceOwner) + " will get " + grantLevelLabel(level) + ": it can " + can + " " + target + ", " + until + "."
}

// writeGrantNote answers a row action without replacing the row: the text
// goes to the status region of the page. unconfirmed marks the row as possibly
// stale (a change was sent and the list could not be read back).
func writeGrantNote(w http.ResponseWriter, id, text string, unconfirmed bool) {
	h := w.Header()
	h.Set("HX-Retarget", "#grant-status")
	h.Set("HX-Reswap", "innerHTML")
	if unconfirmed {
		if b, err := json.Marshal(map[string]any{grantUnconfirmedEvent: map[string]string{"id": id}}); err == nil {
			h.Set("HX-Trigger", string(b))
		}
	}
	writeFragmentTemplate(w, grantTemplates(), "grant_note", text)
}

func findGrantRow(rows []GrantRow, id string) (GrantRow, bool) {
	for _, g := range rows {
		if g.ID == id {
			return g, true
		}
	}
	return GrantRow{}, false
}

// handleGrantChange applies one action. A row action answers the row as the
// Hub has it now; "add" answers a status line for the dialog and asks the
// list to reload.
func handleGrantChange(w http.ResponseWriter, r *http.Request, c OpCaller, u pageUser) {
	f := r.PostForm
	action := r.PathValue("action")
	switch action {
	case "add":
		handleGrantAdd(w, r, c, u)
		return
	case "set-level", "revoke", "approve", "show":
	default:
		http.NotFound(w, r)
		return
	}
	if action == "approve" && !u.Admin {
		http.NotFound(w, r)
		return
	}
	// Every row action names its grant and, to write, the revision it saw.
	// Without them nothing is sent: grant_set would create a grant and
	// grant_revoke would delete whatever is there.
	id := strings.TrimSpace(f.Get("id"))
	if id == "" {
		writeGrantNote(w, "", "Nothing was changed: the row did not name its grant. Reload the list.", false)
		return
	}
	rev, revErr := strconv.ParseUint(f.Get("if_revision"), 10, 64)
	if action != "show" && (revErr != nil || rev == 0) {
		writeGrantNote(w, id, "Nothing was changed: the row did not say which version of the grant it shows. Reload the list.", false)
		return
	}
	var (
		callErr error
		saved   = "Saved"
	)
	switch action {
	case "show":
		saved = ""
	case "set-level":
		level := f.Get("level")
		if _, ok := ops.GrantLevelRank(level); !ok {
			writeGrantNote(w, id, "Nothing was changed: choose a level.", false)
			return
		}
		// The change is built from the grant as the Hub lists it, not from
		// what the form carried.
		rows, err := grantRows(r, c, u, "")
		if err != nil {
			writeGrantNote(w, id, "Nothing was changed: "+failureText(err), false)
			return
		}
		cur, ok := findGrantRow(rows, id)
		switch {
		case !ok:
			writeFragmentTemplate(w, grantTemplates(), "grant_row", GrantRow{Grant: ops.Grant{ID: id}, Gone: true, Error: "Nothing was changed."})
			return
		case cur.Masked():
			cur.Error = "This grant names a session you may not see: it can only be revoked."
		case !cur.CanEdit():
			cur.Error = "Nothing was changed: only an administrator, or the owner of both Workers of a grant an owner decided, can change its level."
		case cur.Revision != rev:
			cur.Error = grantConflictText
		case f.Get("confirm") != "1" && grantRank(level) > grantRank(cur.Level) && grantWidens(level, cur.TargetSession, cur.SourceOwner, cur.TargetOwner):
			until := "with no end"
			if cur.ExpiresAt != nil && !cur.ExpiresAt.IsZero() {
				until = "until " + cur.UntilText()
			}
			cur.Confirm = grantSentence(cur.SourceSession, cur.SourceWorker(), cur.SourceOwner, cur.TargetSession, cur.TargetWorker(), cur.TargetOwner, level, until)
			cur.ConfirmLevel = level
		}
		if cur.Error != "" || cur.Confirm != "" {
			writeFragmentTemplate(w, grantTemplates(), "grant_row", cur)
			return
		}
		a := ops.GrantSetArgs{
			WriteArgs: newWriteArgs(), ID: id, IfRevision: rev, Level: level,
			SourceWorkerID: cur.SourceWorkerID, SourceSession: cur.SourceSession,
			TargetWorkerID: cur.TargetWorkerID, TargetSession: cur.TargetSession,
			ExpiresAt: cur.ExpiresAtValue(), Note: cur.Note,
		}
		var res ops.GrantSetResult
		callErr = c.CallOp(r, ops.OpGrantSet, a, &res)
	case "revoke":
		// The same rule as the row: the grant as the Hub lists it says who
		// may revoke it.
		rows, err := grantRows(r, c, u, "")
		if err != nil {
			writeGrantNote(w, id, "Nothing was changed: "+failureText(err), false)
			return
		}
		cur, ok := findGrantRow(rows, id)
		switch {
		case !ok:
			writeFragmentTemplate(w, grantTemplates(), "grant_row", GrantRow{Grant: ops.Grant{ID: id}, Gone: true, Error: "Nothing was changed."})
			return
		case !cur.CanRevoke():
			cur.Error = "Nothing was changed: only an administrator or the owner of one of its Workers can revoke this grant."
			writeFragmentTemplate(w, grantTemplates(), "grant_row", cur)
			return
		}
		var res ops.WriteResult
		callErr = c.CallOp(r, ops.OpGrantRevoke, ops.GrantRevokeArgs{WriteArgs: newWriteArgs(), ID: id, IfRevision: rev}, &res)
		saved = "Revoked"
	case "approve":
		var res ops.GrantSetResult
		callErr = c.CallOp(r, ops.OpGrantApprove, ops.GrantApproveArgs{WriteArgs: newWriteArgs(), ID: id, IfRevision: rev}, &res)
		saved = "Approved"
	}
	// Answer with the row as the Hub has it now, whatever happened. When the
	// list cannot be read the row stays on the page, marked as unconfirmed.
	rows, err := grantRows(r, c, u, "")
	if err != nil {
		switch {
		case action == "show":
			writeGrantNote(w, id, "The list could not be read: "+failureText(err), false)
		case callErr != nil:
			writeGrantNote(w, id, "The change was refused ("+grantFailure(callErr, true)+") and the list could not be read after it. Reload the list before you change this grant again.", true)
		default:
			writeGrantNote(w, id, "The change was sent, but the list could not be read after it: the row may show the old state. Reload the list before you change this grant again.", true)
		}
		return
	}
	row, ok := findGrantRow(rows, id)
	if !ok {
		row = GrantRow{Grant: ops.Grant{ID: id}, Admin: u.Admin, Gone: true}
	}
	if callErr != nil {
		row.Error = grantFailure(callErr, true)
	} else {
		row.Saved = saved
	}
	writeFragmentTemplate(w, grantTemplates(), "grant_row", row)
}

// grantAddConfirm is the second step of the Add dialog.
type grantAddConfirm struct {
	Sentence string
	// For binds the confirmation to the values it was shown for: a form that
	// changed afterwards is asked again.
	For string
}

// handleGrantAdd creates one grant from the Add dialog. The target is never
// chosen for the user: target_scope says "one" session or "all" shared
// sessions, and without it nothing is sent.
func handleGrantAdd(w http.ResponseWriter, r *http.Request, c OpCaller, u pageUser) {
	f := r.PostForm
	a := ops.GrantSetArgs{
		WriteArgs: newWriteArgs(), SourceWorkerID: f.Get("source_worker_id"), TargetWorkerID: f.Get("target_worker_id"),
		Level: f.Get("level"), Note: strings.TrimSpace(f.Get("note")),
	}
	sw, ss := splitSession(f.Get("source_session"))
	tw, ts := splitSession(f.Get("target_session"))
	scope := f.Get("target_scope")
	if scope == "all" {
		tw, ts = "", ""
	}
	_, levelOK := ops.GrantLevelRank(a.Level)
	switch {
	case a.SourceWorkerID == "" || ss == "":
		writeResultError(w, "Choose the source Worker and its session.")
		return
	case a.TargetWorkerID == "":
		writeResultError(w, "Choose the target Worker.")
		return
	case scope != "one" && scope != "all":
		writeResultError(w, "Choose the target: one session, or every shared session of the target Worker.")
		return
	case scope == "one" && ts == "":
		writeResultError(w, "Choose the target session.")
		return
	case sw != "" && sw != a.SourceWorkerID:
		writeResultError(w, "The source session is not on the chosen source Worker.")
		return
	case tw != "" && tw != a.TargetWorkerID:
		writeResultError(w, "The target session is not on the chosen target Worker.")
		return
	case !levelOK:
		writeResultError(w, "Choose a level.")
		return
	}
	a.SourceSession, a.TargetSession = ss, ts
	until, untilText := f.Get("until"), "with no end"
	switch until {
	case "7", "30":
		days, _ := strconv.Atoi(until)
		a.ExpiresAt = time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour).Format("2006-01-02T15:04:05.000Z")
		untilText = "for " + until + " days"
	default:
		until = ""
	}
	// The second step: a grant that widens access is named in one sentence
	// and sent only when the same values come back confirmed.
	sources, targets := grantWorkers(r, u)
	src, tgt := grantWorkerOption(sources, a.SourceWorkerID), grantWorkerOption(targets, a.TargetWorkerID)
	if grantWidens(a.Level, ts, src.Owner, tgt.Owner) {
		sum := sha256.Sum256([]byte(strings.Join([]string{a.SourceWorkerID, ss, a.TargetWorkerID, scope, ts, a.Level, until}, "\x00")))
		key := hex.EncodeToString(sum[:])
		if f.Get("confirm") != "1" || f.Get("confirm_for") != key {
			writeFragmentTemplate(w, grantTemplates(), "grant_add_confirm", grantAddConfirm{
				Sentence: grantSentence(ss, src.Name, src.Owner, ts, tgt.Name, tgt.Owner, a.Level, untilText), For: key})
			return
		}
	}
	var res ops.GrantSetResult
	if err := c.CallOp(r, ops.OpGrantSet, a, &res); err != nil {
		writeResultError(w, grantFailure(err, false))
		return
	}
	w.Header().Set("HX-Trigger", grantsChangedEvent)
	writeFragment(w, []byte(`<p class="grant-status" role="status">Saved</p>`))
}

// grantWorkerOption finds a Worker of the dialog; an unknown one keeps its id
// as the name and has no owner (which asks for the confirmation).
func grantWorkerOption(list []GrantWorkerOption, id string) GrantWorkerOption {
	if o, ok := findGrantWorker(list, id); ok {
		return o
	}
	return GrantWorkerOption{ID: id, Name: id}
}

func init() {
	RegisterPage(Page{Pattern: pathAccessGrants, Title: "Access", Template: "access_grants.html", Data: userPageData(grantsData)})
	RegisterRoute(Route{Pattern: "POST " + pathAccessGrants + "/{action}", Handler: userRoute(handleGrantChange)})
}
