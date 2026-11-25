package web

// Access (the landing page of the Access group): the read-only matrix "Who
// reaches what". Rows are the sessions that reach other Workers, columns the
// sessions they reach, grouped by Worker ("what <- who" swaps the axes). The
// page changes nothing; editing stays in Grants and Minion control. Every
// choice of the toolbar and the open or collapsed Worker groups live in the
// query string, so the page works without scripts and Refresh keeps them.

import (
	"encoding/csv"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const (
	pathAccess    = "/access"
	pathAccessCSV = "/access.csv"
	// accessOpenColumns: with at most this many session columns every Worker
	// group starts open; a larger matrix starts with every group collapsed.
	accessOpenColumns = 40
	// accessDirWhat is the value of dir= for "what <- who" (targets in rows).
	accessDirWhat = "what"
)

// AccessPage is what access.html sees as .Data.
type AccessPage struct {
	AsOf    template.HTML
	Columns []AccessColumnGroup
	Groups  []AccessSourceGroup
	// List is the long form for a narrow container: every pair, by source.
	List []AccessListGroup
	// Empty: no session reaches (or is blocked from) another Worker.
	Empty bool
	// NoRows: the filters hide every row.
	NoRows bool
	// The toolbar state, as read from the query string.
	Q, Owner   string
	OnlyAccess bool
	Swap       bool
	Owners     []string
	// GroupState is the explicit open ("o:<id>") or collapsed ("c:<id>") state
	// of the column groups, carried by the form as hidden g fields.
	GroupState []string
	// Self is this view's address (Refresh), CSV the same view as a file.
	Self, CSV string
}

// AccessColumnGroup is one Worker on the column axis and its sessions. A
// collapsed group (Open false) is one column with an aggregate per row.
type AccessColumnGroup struct {
	ID, Worker, Owner string
	Online, Open      bool
	Sessions          []AccessColumn
	// Span is the number of table columns the group takes now.
	Span int
	// Toggle is the g value that flips the group.
	Toggle string
}

// AccessColumn is one session on the column axis.
type AccessColumn struct {
	Session, Proof string
	Source         bool
}

// AccessSourceGroup is one Worker on the row axis and its rows.
type AccessSourceGroup struct {
	Worker, Owner string
	Rows          []AccessRow
	Span          int
}

// AccessRow is one session on the row axis: a source, or a target when the
// axes are swapped (Source false: no proof chip).
type AccessRow struct {
	Session, Proof string
	Master, Source bool
	Filter         string
	Cells          []AccessCellView
}

// AccessCellView is one cell: Kind is "ok", "blocked", "none", "same" or
// "agg" (the aggregate of a collapsed group, Glyph "3/6"). Glyph is "C★" for
// master reach and "V", "M" or "C" for a grant; Href leads to the explanation
// of the pair (empty when a session name is hidden). Key and State are what
// access.js compares after a Refresh.
type AccessCellView struct {
	Kind, Target, Reason string
	Glyph, Title, Spoken string
	Href                 string
	Key, State           string
}

// AccessListGroup is one source session in the long form.
type AccessListGroup struct {
	Session, Worker, Owner, Proof string
	Master                        bool
	Filter                        string
	Pairs                         []AccessListPair
}

// AccessListPair is one line "boss on boss-box -> b1 on ci: Control".
type AccessListPair struct {
	Source, Target string
	// Level is what is in effect; Reason is set instead when a layer blocks
	// the pair, and Granted then names the level that was granted.
	Level, Reason, Granted string
	Via                    string
	Href, Key, State       string
}

// accessWhyHref is the link from a cell to its explanation.
func accessWhyHref(src ops.AccessSource, tg ops.AccessTarget) string {
	if src.Masked || tg.Masked {
		return ""
	}
	return pathAccessWhy + "?" + url.Values{"source": {src.WorkerID + "|" + src.Session}, "target": {tg.WorkerID + "|" + tg.Session}}.Encode()
}

func accessLevelLabel(level string) string {
	switch level {
	case ops.AccessLevelView:
		return "View"
	case ops.AccessLevelMessage:
		return "Message"
	}
	return "Control"
}

// accessBlockText names the blocking layer for people.
func accessBlockText(code string) string {
	switch code {
	case ops.AccessBlockedSuspended:
		return "the grant is suspended"
	case ops.AccessBlockedExpired:
		return "the grant has expired"
	case ops.AccessBlockedAuthority:
		return "the grant lost its authority"
	case ops.AccessBlockedOwnerRule:
		return "the owner of the source Worker may not reach this Worker"
	case ops.AccessBlockedWorkerPending:
		return "a Worker is waiting for approval"
	case ops.AccessBlockedNotShared:
		return "the session is not shared"
	case ops.AccessBlockedWorkerOffline:
		return "a Worker is offline"
	case ops.AccessBlockedPreviewOnlyUnknown:
		return "the target may be preview-only"
	}
	return "a layer blocks it"
}

// accessQuery is the state of the page that lives in the query string:
// q (row filter), owner, access=1 (only rows with access), dir=what (targets
// in rows) and g, repeated: "o:<worker id>" opens a column group,
// "c:<worker id>" collapses it, the last one for a Worker wins.
type accessQuery struct {
	Q, Owner   string
	OnlyAccess bool
	Swap       bool
	groups     map[string]bool
}

func parseAccessQuery(v url.Values) accessQuery {
	q := accessQuery{Q: strings.TrimSpace(v.Get("q")), Owner: v.Get("owner"), OnlyAccess: v.Get("access") == "1",
		Swap: v.Get("dir") == accessDirWhat, groups: map[string]bool{}}
	for _, g := range v["g"] {
		if id, ok := strings.CutPrefix(g, "o:"); ok && id != "" {
			q.groups[id] = true
		} else if id, ok := strings.CutPrefix(g, "c:"); ok && id != "" {
			q.groups[id] = false
		}
	}
	return q
}

// accessEnd is one session on an axis: a source or a target.
type accessEnd struct {
	WorkerID, Worker, Owner, Session, Proof string
	Online, Master, Source                  bool
}

func (e accessEnd) name() string { return e.Session + " on " + e.Worker }
func (e accessEnd) key() string  { return e.WorkerID + "|" + e.Session }

// group identifies the Worker of an end on its axis.
func (e accessEnd) group() string {
	if e.WorkerID != "" {
		return e.WorkerID
	}
	return e.Worker + "|" + e.Owner
}

// accessGrid is the matrix with the filters applied: rows and cols are the
// two axes in the chosen direction, show says which rows the filters keep.
type accessGrid struct {
	res        ops.AccessMatrixResult
	src, tg    []accessEnd
	rows, cols []accessEnd
	swap       bool
	cells      map[[2]int]ops.AccessCell // [source, target]
	show       []bool
}

func newAccessGrid(res ops.AccessMatrixResult, q accessQuery) *accessGrid {
	g := &accessGrid{res: res, swap: q.Swap, cells: map[[2]int]ops.AccessCell{}}
	for _, s := range res.Sources {
		g.src = append(g.src, accessEnd{WorkerID: s.WorkerID, Worker: s.WorkerName, Owner: s.Owner, Session: s.Session, Proof: s.Proof, Online: true, Master: s.Master, Source: true})
	}
	for _, t := range res.Targets {
		g.tg = append(g.tg, accessEnd{WorkerID: t.WorkerID, Worker: t.WorkerName, Owner: t.Owner, Session: t.Session, Online: t.Online})
	}
	for _, c := range res.Cells {
		if c.Source < 0 || c.Source >= len(g.src) || c.Target < 0 || c.Target >= len(g.tg) || g.src[c.Source].WorkerID == g.tg[c.Target].WorkerID {
			continue
		}
		g.cells[[2]int{c.Source, c.Target}] = c
	}
	g.rows, g.cols = g.src, g.tg
	if q.Swap {
		g.rows, g.cols = g.tg, g.src
	}
	needle := strings.ToLower(q.Q)
	g.show = make([]bool, len(g.rows))
	for i, r := range g.rows {
		ok := q.Owner == "" || r.Owner == q.Owner
		if ok && needle != "" {
			ok = strings.Contains(strings.ToLower(r.Session+" "+r.Worker+" "+r.Owner), needle)
		}
		if ok && q.OnlyAccess {
			ok = false
			for j := range g.cols {
				if c, has := g.cell(i, j); has && c.InEffect != "" {
					ok = true
					break
				}
			}
		}
		g.show[i] = ok
	}
	return g
}

// at maps a row and a column to the source and target index.
func (g *accessGrid) at(row, col int) (si, ti int) {
	if g.swap {
		return col, row
	}
	return row, col
}

func (g *accessGrid) cell(row, col int) (ops.AccessCell, bool) {
	si, ti := g.at(row, col)
	c, ok := g.cells[[2]int{si, ti}]
	return c, ok
}

// accessPair is one source-target pair that has a cell.
type accessPair struct {
	si, ti int
	cell   ops.AccessCell
}

// pairs is the long form of the rows the filters keep: by source, then target.
func (g *accessGrid) pairs() []accessPair {
	var out []accessPair
	for si := range g.src {
		for ti := range g.tg {
			c, ok := g.cells[[2]int{si, ti}]
			row := si
			if g.swap {
				row = ti
			}
			if ok && g.show[row] {
				out = append(out, accessPair{si, ti, c})
			}
		}
	}
	return out
}

func accessPairKey(src, tg accessEnd) string { return src.key() + ">" + tg.key() }

// accessCellState is what a Refresh compares: the level and how it is held,
// or the blocking layer.
func accessCellState(c ops.AccessCell) string {
	if c.InEffect == "" {
		return "blocked:" + c.BlockedBy
	}
	if c.Via == ops.AccessViaMaster {
		return c.InEffect + "/master"
	}
	return c.InEffect + "/grant"
}

// view is the cell of one row and one column.
func (g *accessGrid) view(row, col int) AccessCellView {
	si, ti := g.at(row, col)
	src, tg := g.src[si], g.tg[ti]
	v := AccessCellView{Kind: "none", Target: tg.name(), Key: accessPairKey(src, tg), State: "none"}
	c, ok := g.cells[[2]int{si, ti}]
	switch {
	case src.WorkerID == tg.WorkerID:
		v.Kind, v.Key, v.State = "same", "", ""
	case ok && c.InEffect != "":
		v.Kind, v.State, v.Href = "ok", accessCellState(c), accessWhyHref(g.res.Sources[si], g.res.Targets[ti])
		label, what := accessLevelLabel(c.InEffect), " of "+tg.name()
		if g.swap {
			what = " for " + src.name()
		}
		if c.Via == ops.AccessViaMaster {
			v.Glyph, v.Title, v.Spoken = "C★", label+what+", via master", "control via master"
		} else {
			v.Glyph, v.Title, v.Spoken = label[:1], label+what+", by a grant", strings.ToLower(label)+" by a grant"
		}
	case ok:
		v.Kind, v.State, v.Reason, v.Href = "blocked", accessCellState(c), accessBlockText(c.BlockedBy), accessWhyHref(g.res.Sources[si], g.res.Targets[ti])
	}
	return v
}

// aggregate is the one cell of a row under a collapsed column group: how
// many of the group's sessions the pair is in effect for ("3/6").
func (g *accessGrid) aggregate(row int, grp AccessColumnGroup, first int) AccessCellView {
	r := g.rows[row]
	if r.WorkerID == g.cols[first].WorkerID {
		return AccessCellView{Kind: "same"}
	}
	var ok, blocked int
	for j := first; j < first+len(grp.Sessions); j++ {
		if c, has := g.cell(row, j); has && c.InEffect != "" {
			ok++
		} else if has {
			blocked++
		}
	}
	v := AccessCellView{Kind: "agg", Glyph: fmt.Sprintf("%d/%d", ok, len(grp.Sessions)), Key: r.key() + ">" + grp.ID + "|*"}
	v.Title = fmt.Sprintf("%d of %d sessions on %s in effect", ok, len(grp.Sessions), grp.Worker)
	if blocked > 0 {
		v.Title += fmt.Sprintf(", %d blocked", blocked)
	}
	v.Spoken, v.State = v.Title, fmt.Sprintf("%d/%d/%d", ok, blocked, len(grp.Sessions))
	if ok > 0 {
		v.Reason = "some"
	}
	return v
}

func accessPage(res ops.AccessMatrixResult, q accessQuery) AccessPage {
	p := AccessPage{Empty: len(res.Cells) == 0, Q: q.Q, Owner: q.Owner, OnlyAccess: q.OnlyAccess, Swap: q.Swap}
	t := time.Time(res.AsOf)
	if !t.IsZero() {
		ms := t.UnixMilli()
		p.AsOf = checkedTime(&ms, "")
	}
	g := newAccessGrid(res, q)

	// Columns: one group per Worker, open or collapsed.
	defaultOpen := len(g.cols) <= accessOpenColumns
	var firsts []int
	for j, c := range g.cols {
		if n := len(p.Columns); n == 0 || p.Columns[n-1].ID != c.group() {
			p.Columns = append(p.Columns, AccessColumnGroup{ID: c.group(), Worker: c.Worker, Owner: c.Owner, Online: c.Online})
			firsts = append(firsts, j)
		}
		grp := &p.Columns[len(p.Columns)-1]
		grp.Sessions = append(grp.Sessions, AccessColumn{Session: c.Session, Proof: c.Proof, Source: c.Source})
	}
	state := url.Values{}
	for i := range p.Columns {
		grp := &p.Columns[i]
		grp.Open = defaultOpen
		if open, set := q.groups[grp.ID]; set {
			grp.Open = open
			p.GroupState = append(p.GroupState, map[bool]string{true: "o:", false: "c:"}[open]+grp.ID)
		}
		grp.Span, grp.Toggle = 1, "o:"+grp.ID
		if grp.Open {
			grp.Span, grp.Toggle = len(grp.Sessions), "c:"+grp.ID
		}
	}

	// Rows the filters keep, grouped by Worker.
	owners := map[string]bool{}
	last := ""
	for i, r := range g.rows {
		owners[r.Owner] = true
		if !g.show[i] {
			continue
		}
		row := AccessRow{Session: r.Session, Proof: r.Proof, Master: r.Master, Source: r.Source, Filter: r.Session + " " + r.Worker + " " + r.Owner}
		for k, grp := range p.Columns {
			if !grp.Open {
				row.Cells = append(row.Cells, g.aggregate(i, grp, firsts[k]))
				continue
			}
			for j := firsts[k]; j < firsts[k]+len(grp.Sessions); j++ {
				row.Cells = append(row.Cells, g.view(i, j))
			}
		}
		if len(p.Groups) == 0 || last != r.group() {
			p.Groups = append(p.Groups, AccessSourceGroup{Worker: r.Worker, Owner: r.Owner, Span: len(row.Cells)})
			last = r.group()
		}
		grp := &p.Groups[len(p.Groups)-1]
		grp.Rows = append(grp.Rows, row)
	}
	p.NoRows = len(p.Groups) == 0
	if q.Owner != "" {
		owners[q.Owner] = true
	}
	for o := range owners {
		p.Owners = append(p.Owners, o)
	}
	sort.Strings(p.Owners)

	// The long form: every pair of the kept rows, by source.
	pairs := g.pairs()
	for si, src := range g.src {
		lg := AccessListGroup{Session: src.Session, Worker: src.Worker, Owner: src.Owner, Proof: src.Proof, Master: src.Master, Filter: src.Session + " " + src.Worker + " " + src.Owner}
		for _, pr := range pairs {
			if pr.si != si {
				continue
			}
			tg := g.tg[pr.ti]
			lp := AccessListPair{Source: src.name(), Target: tg.name(), Via: "by a grant", Granted: accessLevelLabel(pr.cell.Granted),
				Href: accessWhyHref(res.Sources[si], res.Targets[pr.ti]), Key: accessPairKey(src, tg), State: accessCellState(pr.cell)}
			if pr.cell.Via == ops.AccessViaMaster {
				lp.Via = "via master"
			}
			if pr.cell.InEffect != "" {
				lp.Level = accessLevelLabel(pr.cell.InEffect)
			} else {
				lp.Reason = accessBlockText(pr.cell.BlockedBy)
			}
			lg.Pairs = append(lg.Pairs, lp)
		}
		if len(lg.Pairs) > 0 || (!g.swap && g.show[si]) {
			p.List = append(p.List, lg)
		}
	}

	if q.Q != "" {
		state.Set("q", q.Q)
	}
	if q.Owner != "" {
		state.Set("owner", q.Owner)
	}
	if q.OnlyAccess {
		state.Set("access", "1")
	}
	if q.Swap {
		state.Set("dir", accessDirWhat)
	}
	for _, s := range p.GroupState {
		state.Add("g", s)
	}
	p.Self, p.CSV = pathAccess, pathAccessCSV
	if enc := state.Encode(); enc != "" {
		p.Self, p.CSV = pathAccess+"?"+enc, pathAccessCSV+"?"+enc
	}
	return p
}

func accessData(r *http.Request, c OpCaller, _ pageUser) (any, error) {
	var res ops.AccessMatrixResult
	if err := c.CallOp(r, ops.OpAccessMatrix, ops.AccessMatrixArgs{}, &res); err != nil {
		return nil, err
	}
	return accessPage(res, parseAccessQuery(r.URL.Query())), nil
}

// accessCSVHeader names the columns of the export, one line per pair.
var accessCSVHeader = []string{"source worker", "source session", "proof", "target worker", "target session", "granted", "in effect", "via", "blocked by"}

// accessCSVField keeps a spreadsheet from reading a name as a formula.
func accessCSVField(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// handleAccessCSV is GET /access.csv: the pairs of the rows the page shows
// with the same filters, in the long form.
func handleAccessCSV(w http.ResponseWriter, r *http.Request, c OpCaller, _ pageUser) {
	var res ops.AccessMatrixResult
	if err := c.CallOp(r, ops.OpAccessMatrix, ops.AccessMatrixArgs{}, &res); err != nil {
		http.Error(w, "The access matrix could not be read.", http.StatusBadGateway)
		return
	}
	g := newAccessGrid(res, parseAccessQuery(r.URL.Query()))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="access.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	cw := csv.NewWriter(w)
	_ = cw.Write(accessCSVHeader)
	for _, p := range g.pairs() {
		src, tg := g.src[p.si], g.tg[p.ti]
		rec := []string{src.Worker, src.Session, src.Proof, tg.Worker, tg.Session, p.cell.Granted, p.cell.InEffect, p.cell.Via, p.cell.BlockedBy}
		for i := range rec {
			rec[i] = accessCSVField(rec[i])
		}
		_ = cw.Write(rec)
	}
	cw.Flush()
}

func init() {
	RegisterRoute(Route{Pattern: pathAccessCSV, Handler: userRoute(handleAccessCSV)})
}

const pathAccessWhy = "/access/why"

// AccessWhyPage is what access_why.html sees as .Data.
type AccessWhyPage struct {
	Workers        []AccessWhyGroup
	Source, Target string // the selected "<worker id>|<session>"
	// Asked: both ends were chosen. Error is set when the pair has no answer.
	Asked   bool
	Error   string
	Summary string
	Checks  []AccessWhyCheck
}

// AccessWhyGroup is one Worker and its sessions in the two selects.
type AccessWhyGroup struct {
	Worker   string
	Sessions []AccessWhyOption
}

// AccessWhyOption is one session; Value is "<worker id>|<session>".
type AccessWhyOption struct{ Value, Session string }

// AccessWhyCheck is one layer of the answer; Mark says the state in words.
type AccessWhyCheck struct {
	ops.AccessCheck
	Mark string
}

func accessCheckMark(state string) string {
	switch state {
	case ops.AccessCheckPass:
		return "✔ pass"
	case ops.AccessCheckFail:
		return "✘ fail"
	case ops.AccessCheckSkip:
		return "– not evaluated"
	}
	return "? unknown"
}

// accessWhySummary is the one sentence above the checks.
func accessWhySummary(res ops.AccessExplainResult) string {
	src, tg := res.Source.Session+" on "+res.Source.WorkerName, res.Target.Session+" on "+res.Target.WorkerName
	switch res.InEffect {
	case ops.AccessLevelControl:
		return src + " can type into " + tg + "."
	case ops.AccessLevelMessage:
		return src + " can send messages to " + tg + "."
	case ops.AccessLevelView:
		return src + " can view " + tg + "."
	}
	for _, c := range res.Checks {
		if c.State == ops.AccessCheckFail {
			return src + " cannot reach " + tg + ": " + c.Text + "."
		}
	}
	return src + " cannot reach " + tg + "."
}

func accessWhyData(r *http.Request, c OpCaller, _ pageUser) (any, error) {
	var m ops.AccessMatrixResult
	if err := c.CallOp(r, ops.OpAccessMatrix, ops.AccessMatrixArgs{}, &m); err != nil {
		return nil, err
	}
	p := AccessWhyPage{Source: r.URL.Query().Get("source"), Target: r.URL.Query().Get("target")}
	seen := map[string]bool{}
	add := func(workerID, worker, session string, masked bool) {
		v := workerID + "|" + session
		if masked || seen[v] {
			return
		}
		seen[v] = true
		for i := range p.Workers {
			if p.Workers[i].Worker == worker {
				p.Workers[i].Sessions = append(p.Workers[i].Sessions, AccessWhyOption{v, session})
				return
			}
		}
		p.Workers = append(p.Workers, AccessWhyGroup{Worker: worker, Sessions: []AccessWhyOption{{v, session}}})
	}
	for _, s := range m.Sources {
		add(s.WorkerID, s.WorkerName, s.Session, s.Masked)
	}
	for _, t := range m.Targets {
		add(t.WorkerID, t.WorkerName, t.Session, t.Masked)
	}
	sw, ss, okS := strings.Cut(p.Source, "|")
	tw, ts, okT := strings.Cut(p.Target, "|")
	if !okS || !okT || sw == "" || ss == "" || tw == "" || ts == "" {
		return p, nil
	}
	p.Asked = true
	var res ops.AccessExplainResult
	if err := c.CallOp(r, ops.OpAccessExplain, ops.AccessExplainArgs{SourceWorkerID: sw, SourceSession: ss, TargetWorkerID: tw, TargetSession: ts}, &res); err != nil {
		p.Error = "There is no answer for this pair: a Worker is unknown, or neither Worker is yours."
		return p, nil
	}
	// A pair that came by link and is not in the lists is still selectable.
	add(res.Source.WorkerID, res.Source.WorkerName, ss, res.Source.Masked)
	add(res.Target.WorkerID, res.Target.WorkerName, ts, res.Target.Masked)
	p.Summary = accessWhySummary(res)
	for _, ch := range res.Checks {
		p.Checks = append(p.Checks, AccessWhyCheck{ch, accessCheckMark(ch.State)})
	}
	return p, nil
}

func init() {
	RegisterPage(Page{Pattern: pathAccessWhy, Title: "Access", Template: "access_why.html", Data: userPageData(accessWhyData)})
	RegisterPage(Page{Pattern: pathAccess, Title: "Access", Template: "access.html", Data: userPageData(accessData)})
	RegisterNav(NavItem{Label: "Access", Path: pathAccess, Order: onboardingNavOrder + 15, Group: NavGroupAccess, Icon: "i-sessions"})
}
