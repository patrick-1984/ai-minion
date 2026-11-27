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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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
	// AsOf is the moment the Hub rendered this state (the Matrix header).
	AsOf template.HTML
}

// ControlRow is one Worker of the editor.
type ControlRow struct {
	ops.ControlWorker
	// Accepted is the set of Workers this Worker accepts control from: a
	// stored acceptance that allows something (all or some sessions).
	Accepted map[string]bool
	// States is the stored acceptance per source Worker, in its three forms.
	States map[string]AcceptView
	// Others are the other Workers, for the "accepts control from" picker.
	Others []ops.ControlWorker
	// Known are session names seen on the Worker that are not masters yet.
	Known []string
	// Android is true for the Minion Android app: Install apps is offered.
	Android bool
	Saved   string // "Saved" line with the undo of the last change, when set
	Undo    *UndoView
	Note    string // a click that changed nothing, and why
	Error   string
}

// The three forms of a stored acceptance.
const (
	acceptKindAll     = "all"     // no session list: every shared session
	acceptKindLimited = "limited" // only the listed sessions
	acceptKindNone    = "none"    // an empty list: nothing
)

// AcceptView is what a Worker has stored for one source Worker. Kind is
// empty when there is no row.
type AcceptView struct {
	Kind     string
	Sessions []string
	// FilesPull, FilesPush and AppsInstall are the three boxes of the row.
	FilesPull, FilesPush, AppsInstall bool
}

func acceptView(a ops.ControlAccept) AcceptView {
	v := AcceptView{Kind: acceptKindLimited, FilesPull: a.FilesPull, FilesPush: a.FilesPush, AppsInstall: a.AppsInstall}
	switch {
	case a.Sessions == nil:
		v.Kind = acceptKindAll
	case len(*a.Sessions) == 0:
		v.Kind = acceptKindNone
	default:
		v.Sessions = slices.Clone(*a.Sessions)
	}
	return v
}

// AcceptBadge marks an acceptance that is not the whole Worker.
type AcceptBadge struct {
	Source, Tone, Text, Label string
	// Flags are the boxes of a limited exception (Exceptions only).
	Flags *AcceptFlags
}

func sessionCount(n int) string {
	if n == 1 {
		return "1 session"
	}
	return strconv.Itoa(n) + " sessions"
}

// Seen is the stored acceptance from the source Worker as this row renders it,
// in the words of control_set_accept's if_state. Every accept form sends it
// back, so the Hub refuses a click made on a state someone else changed.
func (r ControlRow) Seen(sourceID string) string {
	if k := r.States[sourceID].Kind; k != "" {
		return k
	}
	return ops.ControlAcceptAbsent
}

// AcceptFlags are the three boxes of one actual acceptance (whole Worker or
// limited), with the same labels as on a grant. Each box is its own form
// that sends the acceptance as the page shows it: its state and, for a
// limited one, its exact session list.
type AcceptFlags struct {
	WorkerID, SourceWorkerID string
	Kind                     string
	Sessions                 []string
	Boxes                    []AcceptBox
}

// AcceptBox is one box of an acceptance.
type AcceptBox struct {
	Flag, Label, Aria string
	On                bool
}

// Flags are the boxes of the acceptance of the source Worker; nil when there
// is none that allows something. Install apps is offered for an Android
// Worker, or when it is on.
func (r ControlRow) Flags(sourceID, sourceName string) *AcceptFlags {
	st := r.States[sourceID]
	if st.Kind != acceptKindAll && st.Kind != acceptKindLimited {
		return nil
	}
	v := &AcceptFlags{WorkerID: r.WorkerID, SourceWorkerID: sourceID, Kind: st.Kind, Sessions: st.Sessions}
	v.Boxes = []AcceptBox{
		{Flag: "files_pull", Label: "Receive files from target", On: st.FilesPull,
			Aria: "Master sessions of " + sourceName + " may receive files from " + r.Name},
		{Flag: "files_push", Label: "Send files to target", On: st.FilesPush,
			Aria: "Master sessions of " + sourceName + " may send files to " + r.Name},
	}
	if r.Android || st.AppsInstall {
		v.Boxes = append(v.Boxes, AcceptBox{Flag: "apps_install", Label: "Install apps on " + r.Name, On: st.AppsInstall,
			Aria: "Master sessions of " + sourceName + " may install apps on " + r.Name})
	}
	return v
}

// OnOff is a switch as its forms send it back (if_value).
func (ControlRow) OnOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// seenSwitch reads what a form says the page showed for a switch: nil when
// the form does not say.
func seenSwitch(v string) *bool {
	switch v {
	case "on":
		t := true
		return &t
	case "off":
		f := false
		return &f
	}
	return nil
}

// Badge is the mark of a limited or explicit-nothing acceptance from the
// source Worker; nil for a whole-Worker acceptance and for no row.
func (r ControlRow) Badge(sourceID, sourceName string) *AcceptBadge {
	st := r.States[sourceID]
	switch st.Kind {
	case acceptKindLimited:
		n := len(st.Sessions)
		return &AcceptBadge{Source: sourceName, Tone: "warning", Text: "limited (" + strconv.Itoa(n) + ")",
			Label: r.Name + " accepts only " + sessionCount(n) + " from " + sourceName}
	case acceptKindNone:
		return &AcceptBadge{Source: sourceName, Tone: "danger", Text: "blocked",
			Label: r.Name + " accepts nothing from " + sourceName}
	}
	return nil
}

// Exceptions are the sources a Worker that accepts all Workers still limits
// or blocks (its picker is hidden then).
func (r ControlRow) Exceptions() []AcceptBadge {
	var out []AcceptBadge
	for _, o := range r.Others {
		if b := r.Badge(o.WorkerID, o.Name); b != nil {
			b.Flags = r.Flags(o.WorkerID, o.Name)
			out = append(out, *b)
		}
	}
	return out
}

func (r ControlRow) MasterCount() int {
	n := len(r.Masters)
	for _, m := range r.BoundMasters {
		if m.Master {
			n++
		}
	}
	return n
}

func (r ControlRow) AttachmentStatus(state string) string {
	switch state {
	case "active":
		return "Attached"
	case "waiting":
		return "Waiting for attachment"
	case "offline":
		return "Offline"
	case "unsupported":
		return "Unsupported on this connection"
	case "retired":
		return "Previous attachment retired"
	case "inactive":
		return "Revoked"
	default:
		return "Prepared"
	}
}

func (r ControlRow) CatalogStatus(state string) string {
	switch state {
	case "active":
		return "Last catalog served: master tools"
	case "empty":
		return "Last catalog served: empty"
	case "failed":
		return "Last catalog refresh failed"
	default:
		return "Catalog refresh unconfirmed"
	}
}

// UndoView is the inverse change of the last click.
type UndoView struct {
	Action string
	Fields map[string]string
}

// ControlCheckView is the answer of "Who can control this session?".
type ControlCheckView struct {
	WorkerID        string
	Worker, Session string
	Result          ops.ControlCheckResult
	// Error is set when the Hub could not answer the check; the answer is
	// then unknown, never "no master session".
	Error string
}

var controlTemplates = sync.OnceValue(func() *template.Template {
	return template.Must(template.ParseFS(templateFiles, "templates/minion_control.html"))
})

func controlRows(res ops.ControlListResult, sessions map[string][]string, android map[string]bool) []ControlRow {
	rows := make([]ControlRow, 0, len(res.Workers))
	for _, w := range res.Workers {
		r := ControlRow{ControlWorker: w, Accepted: map[string]bool{}, States: map[string]AcceptView{}, Android: android[w.WorkerID]}
		for _, a := range w.Accepts {
			v := acceptView(a)
			r.States[a.SourceWorkerID] = v
			if v.Kind != acceptKindNone {
				r.Accepted[a.SourceWorkerID] = true
			}
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
		for _, m := range w.BoundMasters {
			masters[m.SessionName] = true
		}
		for _, candidate := range w.MasterCandidates {
			masters[candidate.SessionName] = true
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

// androidWorkers are the Workers the Hub knows as the Minion Android app.
func androidWorkers(r *http.Request) map[string]bool {
	out := map[string]bool{}
	src := currentLiveSource()
	if src == nil {
		return out
	}
	d, err := src.Snapshot(r, true)
	if err != nil {
		return out
	}
	for _, w := range d.Workers {
		if w.Android {
			out[w.ID] = true
		}
	}
	return out
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
		slices.SortFunc(out[k], ops.NaturalCompare)
	}
	return out
}

func controlData(r *http.Request, c OpCaller) (any, error) {
	var res ops.ControlListResult
	if err := c.CallOp(r, ops.OpControlList, ops.ControlListArgs{}, &res); err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	p := ControlPage{Sessions: knownSessions(r), AsOf: checkedTime(&now, "")}
	p.Rows = controlRows(res, p.Sessions, androidWorkers(r))
	for _, row := range p.Rows {
		p.Targets = append(p.Targets, row)
		if row.MasterCount() > 0 {
			p.Sources = append(p.Sources, row)
		}
	}
	w, s := r.URL.Query().Get("check_worker"), r.URL.Query().Get("check_session")
	if c := r.URL.Query().Get("check"); c != "" {
		// One select: "<worker id>|<session>" (ids never hold "|").
		if i := strings.IndexByte(c, '|'); i > 0 {
			w, s = c[:i], c[i+1:]
		}
	}
	if w != "" && s != "" {
		var chk ops.ControlCheckResult
		err := c.CallOp(r, ops.OpControlCheck, ops.ControlCheckArgs{WorkerID: w, SessionName: s}, &chk)
		name := w
		for _, row := range p.Rows {
			if row.WorkerID == w {
				name = row.Name
			}
		}
		p.Check = &ControlCheckView{WorkerID: w, Worker: name, Session: s, Result: chk}
		if err != nil {
			p.Check.Error = failureText(err)
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
		note  string
		// rowErr is a refusal of the page itself (nothing was sent).
		rowErr string
	)
	on := f.Get("value") == "on"
	wa := newWriteArgs()
	flip := map[bool]string{true: "off", false: "on"}[on]
	switch action {
	case "user-managed", "accept-all":
		row = f.Get("worker_id")
		a := ops.ControlSetWorkerArgs{WriteArgs: wa, WorkerID: row}
		// if_value is the switch as the page showed it: the Hub refuses the
		// change when someone else flipped it meanwhile.
		if action == "user-managed" {
			a.UserManaged, a.IfUserManaged = &on, seenSwitch(f.Get("if_value"))
			saved = map[bool]string{true: "People and API keys control this Worker.", false: "Only master sessions control this Worker now."}[on]
		} else {
			a.AcceptAll, a.IfAcceptAll = &on, seenSwitch(f.Get("if_value"))
			saved = map[bool]string{true: "Accepts control from all Workers.", false: "No longer accepts control from all Workers."}[on]
		}
		op, args = ops.OpControlSetWorker, a
		undo = &UndoView{Action: action, Fields: map[string]string{"worker_id": row, "value": flip, "if_value": map[bool]string{true: "on", false: "off"}[on]}}
	case "master":
		row = f.Get("worker_id")
		name := f.Get("session_name")
		// if_master is whether the page showed the session as a master.
		op, args = ops.OpControlSetMaster, ops.ControlSetMasterArgs{WriteArgs: wa, WorkerID: row, SessionName: name, Master: on,
			IfMaster: seenSwitch(f.Get("if_master"))}
		saved = map[bool]string{true: name + " is a master session.", false: name + " is a slave session again."}[on]
		undo = &UndoView{Action: action, Fields: map[string]string{"worker_id": row, "session_name": name, "value": flip,
			"if_master": map[bool]string{true: "on", false: "off"}[on]}}
		if !on && f.Get("legacy") == "1" {
			// Re-adding would drop the migrated limit: no one-click undo.
			undo = nil
			saved += " Adding it again makes it a master without its old limit."
		}
	case "bound-master":
		row = f.Get("worker_id")
		generation, err := strconv.ParseUint(f.Get("if_generation"), 10, 64)
		if err != nil {
			writeResultError(w, "Reload the session to get its current master nomination.")
			return
		}
		name := f.Get("session_name")
		op, args = ops.OpControlSetBoundMaster, ops.ControlSetBoundMasterArgs{
			WriteArgs: wa, WorkerID: row, SessionName: name, IncarnationID: f.Get("incarnation_id"),
			Master: on, IfGeneration: &generation,
		}
		if on {
			saved = "Nominated " + name + " for this attachment."
		} else {
			saved = "Revoked master access for " + name + "."
		}
	case "accept":
		row = f.Get("worker_id")
		src := f.Get("source_worker_id")
		a := ops.ControlSetAcceptArgs{WriteArgs: wa, WorkerID: row, SourceWorkerID: src, Accept: on}
		before, known := storedAccept(r, c, row, src)
		kind := before.View.Kind
		// The precondition of the write: what the page showed when the box
		// was clicked (if_state). A form that does not say falls back to what
		// the Hub has stored now, so the read above and the write below are
		// never two unrelated steps.
		seen := f.Get("if_state")
		if seen == "" && known {
			seen = ops.ControlAcceptAbsent
			if kind != "" {
				seen = kind
			}
		}
		a.IfState = seen
		// Undo is offered only where the inverse click restores exactly what
		// was stored before; it states what this change leaves stored.
		inverse := func(after string) *UndoView {
			return &UndoView{Action: action, Fields: map[string]string{"worker_id": row, "source_worker_id": src, "value": flip, "if_state": after}}
		}
		revoked := ops.ControlAcceptAbsent
		if before.AcceptAll {
			revoked = ops.ControlAcceptNone // a revoke there keeps a row that allows nothing
		}
		readOnly := false
		switch {
		case on && !known:
			// A tick without a list would widen a limited acceptance: nothing
			// is written, and the row is read back with the reason.
			readOnly = true
			rowErr = "Nothing was changed: the Hub could not say what this Worker accepts now. Try again."
		case on && kind == acceptKindLimited && seen == acceptKindLimited:
			// Never widen silently, and never write a list back: another
			// administrator may have changed it since it was read.
			readOnly = true
			note = "This acceptance is limited to " + sessionCount(len(before.View.Sessions)) + ". Change the list with the operation control_set_accept."
		case on && kind == acceptKindNone:
			saved = before.Target + " now accepts every shared session from " + before.Source + "."
			if before.AcceptAll {
				undo = inverse(ops.ControlAcceptAll) // a revoke on a Worker that accepts all blocks again
			} else {
				saved += " No Undo: it cannot block that Worker again."
			}
		case on && kind == acceptKindAll:
			saved = "Accepts control from that Worker, as before."
		case on:
			// No row, or a row the page did not show (the Hub refuses that).
			saved = "Accepts control from that Worker."
			if before.AcceptAll {
				saved += " No Undo: it would block that Worker."
			} else {
				undo = inverse(ops.ControlAcceptAll)
			}
		case !known:
			saved = "No longer accepts control from that Worker. No Undo: the Hub could not say what it accepted before."
		case kind == acceptKindAll:
			saved = "No longer accepts control from that Worker."
			undo = inverse(revoked)
		case kind == acceptKindLimited:
			saved = "No longer accepts control from that Worker. It was limited to some sessions; adding it again accepts all shared sessions, so there is no Undo."
		default:
			// Nothing was allowed before (a blocked entry or none): a tick
			// would accept the whole Worker.
			saved = "No longer accepts control from that Worker. No Undo: it accepted nothing from it before."
		}
		if readOnly {
			break // nothing to save: the row is answered as the Hub has it
		}
		op, args = ops.OpControlSetAccept, a
	case "accept-flag":
		// v0.26.0: one of the three boxes of an actual acceptance, whole
		// Worker or limited. The write keeps the acceptance as the page
		// showed it: its state, and for a limited one its exact session list
		// (a list changed meanwhile is not written back).
		row = f.Get("worker_id")
		src := f.Get("source_worker_id")
		flag := f.Get("flag")
		words := map[string]string{"files_pull": "receive files from", "files_push": "send files to", "apps_install": "install apps on"}[flag]
		if words == "" {
			http.NotFound(w, r)
			return
		}
		before, known := storedAccept(r, c, row, src)
		seen := f.Get("if_state")
		kind := before.View.Kind
		switch {
		case !known:
			rowErr = "Nothing was changed: the Hub could not say what this Worker accepts now. Try again."
		case (kind != acceptKindAll && kind != acceptKindLimited) || seen != kind ||
			(kind == acceptKindLimited && !slices.Equal(sortedNames(f["sessions"]), sortedNames(before.View.Sessions))):
			note = "Nothing was changed: this acceptance was changed meanwhile. This is the current state."
		default:
			a := ops.ControlSetAcceptArgs{WriteArgs: wa, WorkerID: row, SourceWorkerID: src, Accept: true, IfState: kind}
			if kind == acceptKindLimited {
				list := slices.Clone(before.View.Sessions)
				a.Sessions = &list
			}
			switch flag {
			case "files_pull":
				a.FilesPull = &on
			case "files_push":
				a.FilesPush = &on
			default:
				a.AppsInstall = &on
			}
			op, args = ops.OpControlSetAccept, a
			if on {
				saved = "Master sessions of " + before.Source + " may " + words + " " + before.Target + "."
			} else {
				saved = "Master sessions of " + before.Source + " may no longer " + words + " " + before.Target + "."
			}
			if flag == "apps_install" && on {
				saved += " A first install still needs a confirmation on the device."
			}
			if kind == acceptKindAll {
				undo = &UndoView{Action: action, Fields: map[string]string{"worker_id": row, "source_worker_id": src, "flag": flag, "value": flip, "if_state": kind}}
			}
		}
	default:
		http.NotFound(w, r)
		return
	}
	var res ops.WriteResult
	var callErr error
	if op != "" {
		callErr = c.CallOp(r, op, args, &res)
	}
	// Answer with the row as the Hub has it now, whatever happened.
	var list ops.ControlListResult
	if err := c.CallOp(r, ops.OpControlList, ops.ControlListArgs{}, &list); err != nil {
		writeControlReload(w, op != "", callErr, err)
		return
	}
	// The matrix and other rows read the same rows: ask the page to refresh
	// them from the Hub (minion-control.js).
	w.Header().Set("HX-Trigger", "control-changed")
	rows := controlRows(list, knownSessions(r), androidWorkers(r))
	for _, cr := range rows {
		if cr.WorkerID != row {
			continue
		}
		switch {
		case callErr != nil:
			cr.Error = failureText(callErr)
		case rowErr != "":
			cr.Error = rowErr
		default:
			cr.Saved, cr.Undo, cr.Note = saved, undo, note
		}
		writeFragmentTemplate(w, controlTemplates(), "control_row", cr)
		return
	}
	writeResultError(w, "The Worker is gone.")
}

// controlReloadEvent asks the page to reload itself from the Hub
// (minion-control.js): the row and its checkbox may no longer show what the
// Hub has.
const controlReloadEvent = "control-reload"

// writeControlReload answers a click whose row could not be read back: the
// row stays as it is, the text goes to the status region of the page and the
// page reloads what the Hub has.
func writeControlReload(w http.ResponseWriter, sent bool, callErr, readErr error) {
	why := strings.TrimRight(failureText(readErr), ". ") + ". Reloading what the Hub has saved."
	var text string
	switch {
	case callErr != nil:
		text = "The change was refused (" + strings.TrimRight(failureText(callErr), ". ") + ") and the Workers could not be read after it: " + why
	case sent:
		text = "The change was sent, but the Workers could not be read after it: " + why
	default:
		text = "Nothing was changed, and the Workers could not be read: " + why
	}
	h := w.Header()
	h.Set("HX-Retarget", "#control-status")
	h.Set("HX-Reswap", "innerHTML")
	h.Set("HX-Trigger", controlReloadEvent)
	writeFragment(w, []byte(template.HTMLEscapeString(text)))
}

// acceptBefore is what the target has stored for the source before a click.
type acceptBefore struct {
	View           AcceptView // Kind is empty without a row
	AcceptAll      bool       // the target accepts all Workers
	Target, Source string     // names, for the saved sentence
}

// storedAccept reads what target accepts from source now; false when the Hub
// could not say.
func storedAccept(r *http.Request, c OpCaller, target, source string) (acceptBefore, bool) {
	out := acceptBefore{Target: target, Source: source}
	var list ops.ControlListResult
	if err := c.CallOp(r, ops.OpControlList, ops.ControlListArgs{}, &list); err != nil {
		return out, false
	}
	for _, w := range list.Workers {
		switch w.WorkerID {
		case source:
			out.Source = w.Name
		case target:
			out.Target, out.AcceptAll = w.Name, w.AcceptAll
			for _, a := range w.Accepts {
				if a.SourceWorkerID == source {
					out.View = acceptView(a)
				}
			}
		}
	}
	return out, true
}

func init() {
	RegisterPage(Page{Pattern: pathMinionControl, Title: "Minion control", Template: "minion_control.html", Data: adminPageData(controlData)})
	RegisterNav(NavItem{Label: "Minion control", Path: pathMinionControl, Order: onboardingNavOrder + 20, Roles: []authz.Role{authz.RoleAdmin}, Group: NavGroupAccess, Icon: "i-sessions"})
	RegisterRoute(Route{Pattern: "POST " + pathMinionControl + "/{action}", Handler: adminRoute(handleControlChange)})
}

// sortedNames is a sorted copy of session names, for comparing two lists.
func sortedNames(names []string) []string {
	out := slices.Clone(names)
	slices.Sort(out)
	return out
}
