package web

// The two read-only "Access" panels (story U4):
//
//   - the view "Access" of the session page: what THIS session can reach and
//     who can reach it, from grant_list (limited to the session's Worker) and
//     the master reach of access_matrix. It is drawn with the page; a Hub that
//     has neither operation leaves the view out.
//   - the panel "Access" of the Worker page, loaded once like the Sharing
//     panel (GET /w/<id>/access, the session routes' guard): for an
//     administrator which Workers' master sessions the Worker accepts
//     (control_list), for the owner and the administrator how many grants touch
//     the Worker.
//
// Nothing is changed from either panel: every link leads to the page that
// owns the change (/access/grants, /access/control) or explains a pair
// (/access/why). The operations decide what the caller sees; a masked session
// is printed as the operation sends it and is never a link.

import (
	"bytes"
	"cmp"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// pathWorkerAccessPattern is the route of the Access panel of a Worker.
const pathWorkerAccessPattern = "/w/{worker}/access"

// WorkerAccessPath returns the path of the Access panel of a Worker, by canonical id.
func WorkerAccessPath(worker string) string { return "/w/" + url.PathEscape(worker) + "/access" }

// Values of AccessPanelRow.Via.
const (
	accessRowGrant  = "grant"
	accessRowMaster = "master"
)

// AccessPanelRow is one row of "Can reach" or "Reachable by": the other side
// of a grant or of a master's reach.
type AccessPanelRow struct {
	// Session is the other session, or the masked text of the operation.
	Session string
	// AllSessions: the grant covers every shared session of the Worker.
	AllSessions bool
	Masked      bool
	Worker      string
	Owner       string
	Level       string
	// State is the grant's state or what the matrix says is in effect; Tone is its badge tone.
	State, Tone string
	// Via is "grant" or "master".
	Via string
	// WhyURL is empty when the pair is not one pair of named sessions.
	WhyURL    string
	GrantsURL string
}

// SessionAccessView is what the "session_access" fragment sees.
type SessionAccessView struct {
	Session, Worker string
	Reach           []AccessPanelRow
	ReachedBy       []AccessPanelRow
	// Notes say what could not be read.
	Notes []string
	// AddURL opens the Grants page with this session as the source; empty
	// for a person who cannot operate the session.
	AddURL    string
	GrantsURL string
}

var accessPanelTemplates = sync.OnceValue(func() *template.Template {
	return template.Must(template.ParseFS(templateFiles, "templates/_session_access.html"))
})

func opUnsupported(err error) bool {
	var oe *OpError
	return errors.As(err, &oe) && oe.Code == "unsupported_op"
}

func accessPair(workerID, session string) string { return workerID + "|" + session }

func accessPairWhy(sourceWorker, sourceSession, targetWorker, targetSession string) string {
	return pathAccessWhy + "?" + url.Values{"source": {accessPair(sourceWorker, sourceSession)}, "target": {accessPair(targetWorker, targetSession)}}.Encode()
}

func accessGrantsOf(workerID string) string {
	return pathAccessGrants + "?" + url.Values{"worker": {workerID}}.Encode()
}

// accessCellState is the state of a pair the matrix knows.
func accessCellState(c ops.AccessCell) (state, tone string) {
	if c.InEffect != "" {
		return "In effect", "positive"
	}
	return "Blocked: " + accessBlockText(c.BlockedBy), "warning"
}

// accessGrantState is the state of a grant row: the grant's own state, made
// exact by the matrix cell of that grant when there is one.
func accessGrantState(g ops.Grant, cell *ops.AccessCell) (state, tone string) {
	switch (GrantRow{Grant: g}).StateKey() {
	case ops.GrantStateSuspended:
		return "Suspended", "danger"
	case ops.GrantStateExpired:
		return "Expired", "info"
	}
	if cell != nil {
		return accessCellState(*cell)
	}
	return "Active", "positive"
}

// sessionAccessView reads the two lists of one session. ok is false when the
// Hub has neither operation: the view is then left out.
func sessionAccessView(r *http.Request, c OpCaller, s SessionContext) (v SessionAccessView, ok bool) {
	v = SessionAccessView{Session: s.Session, Worker: s.Worker, GrantsURL: accessGrantsOf(s.WorkerID)}
	if s.Operate {
		v.AddURL = pathAccessGrants + "?" + url.Values{"source": {accessPair(s.WorkerID, s.Session)}}.Encode() + "#grant-add"
	}
	var grants ops.GrantListResult
	gerr := c.CallOp(r, ops.OpGrantList, ops.GrantListArgs{WorkerID: s.WorkerID}, &grants)
	var m ops.AccessMatrixResult
	merr := c.CallOp(r, ops.OpAccessMatrix, ops.AccessMatrixArgs{WorkerID: s.WorkerID}, &m)
	if opUnsupported(gerr) && opUnsupported(merr) {
		return v, false
	}
	if gerr != nil {
		grants = ops.GrantListResult{}
		v.Notes = append(v.Notes, "The grants could not be read just now. "+truncateRunes(failureText(gerr), 200))
	}
	if merr != nil {
		m = ops.AccessMatrixResult{}
		v.Notes = append(v.Notes, "The reach of master sessions could not be read just now. "+truncateRunes(failureText(merr), 200))
	}

	// The cell of one grant between two named ends, when the matrix has it.
	cellOf := func(grantID, sw, ss, tw, ts string) *ops.AccessCell {
		for i, cell := range m.Cells {
			if cell.Via != grantID || cell.Source >= len(m.Sources) || cell.Target >= len(m.Targets) || cell.Source < 0 || cell.Target < 0 {
				continue
			}
			src, tg := m.Sources[cell.Source], m.Targets[cell.Target]
			if !src.Masked && !tg.Masked && src.WorkerID == sw && src.Session == ss && tg.WorkerID == tw && tg.Session == ts {
				return &m.Cells[i]
			}
		}
		return nil
	}
	for _, g := range grants.Grants {
		gr := GrantRow{Grant: g}
		link := accessGrantsOf(s.WorkerID) + "#grant-" + url.PathEscape(g.ID)
		if g.SourceWorkerID == s.WorkerID && g.SourceSession == s.Session {
			row := AccessPanelRow{Session: g.TargetSession, AllSessions: g.TargetSession == "", Worker: gr.TargetWorker(), Owner: g.TargetOwner,
				Level: grantLevelLabel(g.Level), Via: accessRowGrant, GrantsURL: link}
			var cell *ops.AccessCell
			if !row.AllSessions {
				cell = cellOf(g.ID, s.WorkerID, s.Session, g.TargetWorkerID, g.TargetSession)
				row.WhyURL = accessPairWhy(s.WorkerID, s.Session, g.TargetWorkerID, g.TargetSession)
			}
			row.State, row.Tone = accessGrantState(g, cell)
			v.Reach = append(v.Reach, row)
		}
		if g.TargetWorkerID == s.WorkerID && (g.TargetSession == "" || g.TargetSession == s.Session) {
			row := AccessPanelRow{Session: g.SourceSession, Worker: gr.SourceWorker(), Owner: g.SourceOwner,
				Level: grantLevelLabel(g.Level), Via: accessRowGrant, GrantsURL: link,
				WhyURL: accessPairWhy(g.SourceWorkerID, g.SourceSession, s.WorkerID, s.Session)}
			row.State, row.Tone = accessGrantState(g, cellOf(g.ID, g.SourceWorkerID, g.SourceSession, s.WorkerID, s.Session))
			v.ReachedBy = append(v.ReachedBy, row)
		}
	}
	for _, cell := range m.Cells {
		if cell.Via != ops.AccessViaMaster || cell.Source < 0 || cell.Target < 0 || cell.Source >= len(m.Sources) || cell.Target >= len(m.Targets) {
			continue
		}
		src, tg := m.Sources[cell.Source], m.Targets[cell.Target]
		row := AccessPanelRow{Level: accessLevelLabel(cell.Granted), Via: accessRowMaster, GrantsURL: accessGrantsOf(s.WorkerID), WhyURL: accessWhyHref(src, tg)}
		row.State, row.Tone = accessCellState(cell)
		if !src.Masked && src.WorkerID == s.WorkerID && src.Session == s.Session {
			row.Session, row.Masked, row.Worker, row.Owner = tg.Session, tg.Masked, cmp.Or(tg.WorkerName, tg.WorkerID), tg.Owner
			v.Reach = append(v.Reach, row)
		}
		if !tg.Masked && tg.WorkerID == s.WorkerID && tg.Session == s.Session {
			row.Session, row.Masked, row.Worker, row.Owner = src.Session, src.Masked, cmp.Or(src.WorkerName, src.WorkerID), src.Owner
			v.ReachedBy = append(v.ReachedBy, row)
		}
	}
	for _, rows := range [][]AccessPanelRow{v.Reach, v.ReachedBy} {
		slices.SortStableFunc(rows, func(a, b AccessPanelRow) int {
			return cmp.Or(ops.NaturalCompare(a.Worker, b.Worker), ops.NaturalCompare(a.Session, b.Session), cmp.Compare(a.Via, b.Via))
		})
	}
	return v, true
}

// renderSessionAccess is the session pane "Access".
func renderSessionAccess(r *http.Request, s SessionContext) (template.HTML, error) {
	c, err := sessionCaller()
	if err != nil {
		return "", err
	}
	v, ok := sessionAccessView(r, c, s)
	if !ok {
		return "", nil
	}
	var buf bytes.Buffer
	if err := accessPanelTemplates().ExecuteTemplate(&buf, "session_access", v); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil //nolint:gosec // produced by html/template above
}

// WorkerAcceptEntry is one Worker of "Accepts master sessions from": Mark is
// empty for a whole-Worker acceptance, else "limited (n)" or "blocked".
type WorkerAcceptEntry struct {
	Name, Mark, Tone, Label string
}

// WorkerAccessView is what the "worker_access_panel" fragment sees.
type WorkerAccessView struct {
	WorkerID, Worker string
	// Admin: the acceptance summary is shown (control_list is an administrator operation).
	Admin bool
	// AcceptNote replaces the summary when it could not be read.
	AcceptNote string
	AcceptAll  bool
	// Accepts are the Workers accepted in whole or in part; Exceptions are
	// the limited and blocked ones of a Worker that accepts all Workers, and
	// the blocked ones otherwise.
	Accepts    []WorkerAcceptEntry
	Exceptions []WorkerAcceptEntry
	ControlURL string
	// GrantNote replaces the count when the grants could not be read.
	GrantNote  string
	Grants     int
	GrantsFrom int
	GrantsTo   int
	GrantsURL  string
}

// GrantSentence is the count of grants in words.
func (v WorkerAccessView) GrantSentence() string {
	switch v.Grants {
	case 0:
		return "No grant touches this Worker."
	case 1:
		return "1 grant touches this Worker"
	}
	return strconv.Itoa(v.Grants) + " grants touch this Worker"
}

func workerAccessView(r *http.Request, t target, admin bool) WorkerAccessView {
	v := WorkerAccessView{WorkerID: t.worker.ID, Worker: cmp.Or(t.worker.Name, t.worker.ID), Admin: admin,
		ControlURL: pathMinionControl, GrantsURL: accessGrantsOf(t.worker.ID)}
	if admin {
		var list ops.ControlListResult
		if err := t.c.CallOp(r, ops.OpControlList, ops.ControlListArgs{}, &list); err != nil {
			v.AcceptNote = "What this Worker accepts could not be read just now. " + truncateRunes(failureText(err), 200)
		} else {
			v.accepts(list)
		}
	}
	var grants ops.GrantListResult
	if err := t.c.CallOp(r, ops.OpGrantList, ops.GrantListArgs{WorkerID: t.worker.ID}, &grants); err != nil {
		v.GrantNote = "The grants of this Worker could not be read just now. " + truncateRunes(failureText(err), 200)
		return v
	}
	for _, g := range grants.Grants {
		from, to := g.SourceWorkerID == t.worker.ID, g.TargetWorkerID == t.worker.ID
		if !from && !to {
			continue // the operation filters; a row of another Worker is not counted
		}
		v.Grants++
		if from {
			v.GrantsFrom++
		}
		if to {
			v.GrantsTo++
		}
	}
	return v
}

// accepts fills the acceptance summary of the Worker from control_list.
func (v *WorkerAccessView) accepts(list ops.ControlListResult) {
	names := map[string]string{}
	var own *ops.ControlWorker
	for i, w := range list.Workers {
		names[w.WorkerID] = cmp.Or(w.Name, w.WorkerID)
		if w.WorkerID == v.WorkerID {
			own = &list.Workers[i]
		}
	}
	if own == nil {
		v.AcceptNote = "The Hub does not list what this Worker accepts."
		return
	}
	v.AcceptAll = own.AcceptAll
	for _, a := range own.Accepts {
		e := WorkerAcceptEntry{Name: cmp.Or(names[a.SourceWorkerID], a.SourceWorkerID)}
		switch st := acceptView(a); st.Kind {
		case acceptKindLimited:
			e.Mark, e.Tone = "limited ("+strconv.Itoa(len(st.Sessions))+")", "warning"
			e.Label = v.Worker + " accepts only " + sessionCount(len(st.Sessions)) + " from " + e.Name
		case acceptKindNone:
			e.Mark, e.Tone, e.Label = "blocked", "danger", v.Worker+" accepts nothing from "+e.Name
		}
		switch {
		case e.Mark == "" && own.AcceptAll: // covered by "all Workers"
		case e.Mark == "blocked" || own.AcceptAll:
			v.Exceptions = append(v.Exceptions, e)
		default:
			v.Accepts = append(v.Accepts, e)
		}
	}
	byName := func(a, b WorkerAcceptEntry) int { return ops.NaturalCompare(a.Name, b.Name) }
	slices.SortStableFunc(v.Accepts, byName)
	slices.SortStableFunc(v.Exceptions, byName)
}

// handleWorkerAccess answers the Access panel of the Worker page. A person who
// may not operate the Worker gets the plain 404 of an unknown Worker.
func handleWorkerAccess(w http.ResponseWriter, r *http.Request, t target) {
	if !t.operate {
		http.NotFound(w, r)
		return
	}
	_, admin, _ := requestUser(r)
	writeFragmentTemplate(w, accessPanelTemplates(), "worker_access_panel", workerAccessView(r, t, admin))
}

func init() {
	RegisterSessionPane(SessionPane{ID: "access", Label: "Access", Order: 150, Render: renderSessionAccess})
	RegisterRoute(Route{Pattern: "GET " + pathWorkerAccessPattern, Handler: sessionRoute(handleWorkerAccess)})
}
