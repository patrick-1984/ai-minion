package web

// Access > Minion control (v0.9.0, A17): which sessions are master sessions,
// which Workers accept control from which, and whether people and API keys
// control a Worker (user-managed). Every click saves at once (one POST per
// control) and the answer is the row as the Hub now has it, with an Undo; a
// lost answer reloads the row (minion-control.js), so the page never shows a
// state the Hub does not have. Design: research/minion-access/EXPERIENCE.md.

import (
	"html/template"
	"net/http"
	"sort"
	"sync"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

const pathMinionControl = "/access/control"

// ControlPage is what minion_control.html sees as .Data.
type ControlPage struct {
	Rows []ControlRow
	// Sources are the Workers that have at least one master session: the
	// rows of the matrix. Targets are every Worker: its columns.
	Sources []ControlRow
	Targets []ControlRow
	// Sessions are the known session names per Worker, for the master picker.
	Sessions map[string][]string
	Check    *ControlCheckView
}

// ControlRow is one Worker of the editor.
type ControlRow struct {
	ops.ControlWorker
	// Accepted is the set of Workers this Worker accepts control from.
	Accepted map[string]bool
	// Others are the other Workers, for the "accepts control from" picker.
	Others []ops.ControlWorker
	// Known are session names seen on the Worker that are not masters yet.
	Known []string
	Saved string // "Saved" line with the undo of the last change, when set
	Undo  *UndoView
	Error string
}

// UndoView is the inverse change of the last click.
type UndoView struct {
	Action string
	Fields map[string]string
}

// ControlCheckView is the answer of "Who can control this session?".
type ControlCheckView struct {
	Worker, Session string
	Result          ops.ControlCheckResult
}

var controlTemplates = sync.OnceValue(func() *template.Template {
	return template.Must(template.ParseFS(templateFiles, "templates/minion_control.html"))
})

func controlRows(res ops.ControlListResult, sessions map[string][]string) []ControlRow {
	rows := make([]ControlRow, 0, len(res.Workers))
	for _, w := range res.Workers {
		r := ControlRow{ControlWorker: w, Accepted: map[string]bool{}}
		for _, a := range w.Accepts {
			r.Accepted[a.SourceWorkerID] = true
		}
		for _, o := range res.Workers {
			if o.WorkerID != w.WorkerID {
				r.Others = append(r.Others, o)
			}
		}
		masters := map[string]bool{}
		for _, m := range w.Masters {
			masters[m.SessionName] = true
		}
		for _, n := range sessions[w.WorkerID] {
			if !masters[n] {
				r.Known = append(r.Known, n)
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// knownSessions are the session names the Hub sees on each Worker (shared
// sessions of online Workers).
func knownSessions(r *http.Request) map[string][]string {
	out := map[string][]string{}
	src := currentLiveSource()
	if src == nil {
		return out
	}
	d, err := src.Snapshot(r, true)
	if err != nil {
		return out
	}
	for _, s := range d.Sessions {
		out[s.WorkerID] = append(out[s.WorkerID], s.Name)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func controlData(r *http.Request, c OpCaller) (any, error) {
	var res ops.ControlListResult
	if err := c.CallOp(r, ops.OpControlList, ops.ControlListArgs{}, &res); err != nil {
		return nil, err
	}
	p := ControlPage{Sessions: knownSessions(r)}
	p.Rows = controlRows(res, p.Sessions)
	for _, row := range p.Rows {
		p.Targets = append(p.Targets, row)
		if len(row.Masters) > 0 {
			p.Sources = append(p.Sources, row)
		}
	}
	if w, s := r.URL.Query().Get("check_worker"), r.URL.Query().Get("check_session"); w != "" && s != "" {
		var chk ops.ControlCheckResult
		if err := c.CallOp(r, ops.OpControlCheck, ops.ControlCheckArgs{WorkerID: w, SessionName: s}, &chk); err == nil {
			name := w
			for _, row := range p.Rows {
				if row.WorkerID == w {
					name = row.Name
				}
			}
			p.Check = &ControlCheckView{Worker: name, Session: s, Result: chk}
		}
	}
	return p, nil
}

// handleControlChange applies one click and answers the row of the Worker it
// changed, as the Hub has it now, with the undo of the change.
func handleControlChange(w http.ResponseWriter, r *http.Request, c OpCaller) {
	action := r.PathValue("action")
	f := r.PostForm
	var (
		op    string
		args  any
		row   string
		undo  *UndoView
		saved string
	)
	on := f.Get("value") == "on"
	wa := newWriteArgs()
	flip := map[bool]string{true: "off", false: "on"}[on]
	switch action {
	case "user-managed", "accept-all":
		row = f.Get("worker_id")
		a := ops.ControlSetWorkerArgs{WriteArgs: wa, WorkerID: row}
		if action == "user-managed" {
			a.UserManaged = &on
			saved = map[bool]string{true: "People and API keys control this Worker.", false: "Only master sessions control this Worker now."}[on]
		} else {
			a.AcceptAll = &on
			saved = map[bool]string{true: "Accepts control from all Workers.", false: "No longer accepts control from all Workers."}[on]
		}
		op, args = ops.OpControlSetWorker, a
		undo = &UndoView{Action: action, Fields: map[string]string{"worker_id": row, "value": flip}}
	case "master":
		row = f.Get("worker_id")
		name := f.Get("session_name")
		op, args = ops.OpControlSetMaster, ops.ControlSetMasterArgs{WriteArgs: wa, WorkerID: row, SessionName: name, Master: on}
		saved = map[bool]string{true: name + " is a master session.", false: name + " is a slave session again."}[on]
		undo = &UndoView{Action: action, Fields: map[string]string{"worker_id": row, "session_name": name, "value": flip}}
		if !on && f.Get("legacy") == "1" {
			// Re-adding would drop the migrated limit: no one-click undo.
			undo = nil
			saved += " Adding it again makes it a master without its old limit."
		}
	case "accept":
		row = f.Get("worker_id")
		src := f.Get("source_worker_id")
		op, args = ops.OpControlSetAccept, ops.ControlSetAcceptArgs{WriteArgs: wa, WorkerID: row, SourceWorkerID: src, Accept: on}
		saved = map[bool]string{true: "Accepts control from that Worker.", false: "No longer accepts control from that Worker."}[on]
		undo = &UndoView{Action: action, Fields: map[string]string{"worker_id": row, "source_worker_id": src, "value": flip}}
	default:
		http.NotFound(w, r)
		return
	}
	var res ops.WriteResult
	callErr := c.CallOp(r, op, args, &res)
	// The matrix and other rows read the same rows: ask the page to refresh
	// them from the Hub (minion-control.js).
	w.Header().Set("HX-Trigger", "control-changed")
	// Answer with the row as the Hub has it now, whatever happened.
	var list ops.ControlListResult
	if err := c.CallOp(r, ops.OpControlList, ops.ControlListArgs{}, &list); err != nil {
		writeResultError(w, failureText(err))
		return
	}
	rows := controlRows(list, knownSessions(r))
	for _, cr := range rows {
		if cr.WorkerID != row {
			continue
		}
		if callErr != nil {
			cr.Error = failureText(callErr)
		} else {
			cr.Saved, cr.Undo = saved, undo
		}
		writeFragmentTemplate(w, controlTemplates(), "control_row", cr)
		return
	}
	writeResultError(w, "The Worker is gone.")
}

func init() {
	RegisterPage(Page{Pattern: pathMinionControl, Title: "Minion control", Template: "minion_control.html", Data: adminPageData(controlData)})
	RegisterNav(NavItem{Label: "Minion control", Path: pathMinionControl, Order: onboardingNavOrder + 20, Roles: []authz.Role{authz.RoleAdmin}, Group: NavGroupAccess, Icon: "i-sessions"})
	RegisterRoute(Route{Pattern: "POST " + pathMinionControl + "/{action}", Handler: adminRoute(handleControlChange)})
}
