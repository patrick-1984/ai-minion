package web

// Access (the landing page of the Access group): the read-only matrix "Who
// reaches what". Rows are the sessions that reach other Workers (today:
// master sessions), columns the sessions they reach, grouped by Worker. The
// page changes nothing; editing stays in Minion control.

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const pathAccess = "/access"

// AccessPage is what access.html sees as .Data.
type AccessPage struct {
	AsOf    template.HTML
	Columns []AccessColumnGroup
	Groups  []AccessSourceGroup
	// Empty: no session reaches (or is blocked from) another Worker.
	Empty bool
}

// AccessColumnGroup is one target Worker and its sessions (the columns).
type AccessColumnGroup struct {
	Worker, Owner string
	Online        bool
	Sessions      []string
}

// AccessSourceGroup is one source Worker and its rows.
type AccessSourceGroup struct {
	Worker, Owner string
	Rows          []AccessRow
}

// AccessRow is one source session.
type AccessRow struct {
	ops.AccessSource
	Cells []AccessCellView
	// Reach is the list form: "3 sessions on ci, 1 session on lab".
	Reach string
}

// AccessCellView is one cell: Kind is "ok", "blocked", "none" or "same".
// Glyph is "C★" for master reach and "V", "M" or "C" for a grant; Href leads
// to the explanation of the pair (empty when a session name is hidden).
type AccessCellView struct {
	Kind, Target, Reason string
	Glyph, Title, Spoken string
	Href                 string
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

func accessPage(res ops.AccessMatrixResult) AccessPage {
	p := AccessPage{Empty: len(res.Cells) == 0}
	t := time.Time(res.AsOf)
	if !t.IsZero() {
		ms := t.UnixMilli()
		p.AsOf = checkedTime(&ms, "")
	}
	for _, tg := range res.Targets {
		if n := len(p.Columns); n == 0 || p.Columns[n-1].Worker != tg.WorkerName || p.Columns[n-1].Owner != tg.Owner {
			p.Columns = append(p.Columns, AccessColumnGroup{Worker: tg.WorkerName, Owner: tg.Owner, Online: tg.Online})
		}
		g := &p.Columns[len(p.Columns)-1]
		g.Sessions = append(g.Sessions, tg.Session)
	}
	cells := map[[2]int]ops.AccessCell{}
	for _, c := range res.Cells {
		cells[[2]int{c.Source, c.Target}] = c
	}
	for i, src := range res.Sources {
		row := AccessRow{AccessSource: src}
		var reach []string
		count := map[string]int{}
		for j, tg := range res.Targets {
			v := AccessCellView{Kind: "none", Target: tg.Session + " on " + tg.WorkerName}
			c, ok := cells[[2]int{i, j}]
			switch {
			case tg.WorkerID == src.WorkerID:
				v.Kind = "same"
			case ok && c.InEffect != "":
				v.Kind, v.Href = "ok", accessWhyHref(src, tg)
				label := accessLevelLabel(c.InEffect)
				if c.Via == ops.AccessViaMaster {
					v.Glyph, v.Title, v.Spoken = "C★", label+" of "+v.Target+", via master", "control via master"
				} else {
					v.Glyph, v.Title, v.Spoken = label[:1], label+" of "+v.Target+", by a grant", strings.ToLower(label)+" by a grant"
				}
				if count[tg.WorkerName] == 0 {
					reach = append(reach, tg.WorkerName)
				}
				count[tg.WorkerName]++
			case ok:
				v.Kind, v.Reason, v.Href = "blocked", accessBlockText(c.BlockedBy), accessWhyHref(src, tg)
			}
			row.Cells = append(row.Cells, v)
		}
		for k, name := range reach {
			unit := "sessions"
			if count[name] == 1 {
				unit = "session"
			}
			reach[k] = fmt.Sprintf("%d %s on %s", count[name], unit, name)
		}
		row.Reach = strings.Join(reach, ", ")
		if n := len(p.Groups); n == 0 || p.Groups[n-1].Worker != src.WorkerName || p.Groups[n-1].Owner != src.Owner {
			p.Groups = append(p.Groups, AccessSourceGroup{Worker: src.WorkerName, Owner: src.Owner})
		}
		g := &p.Groups[len(p.Groups)-1]
		g.Rows = append(g.Rows, row)
	}
	return p
}

func accessData(r *http.Request, c OpCaller, _ pageUser) (any, error) {
	var res ops.AccessMatrixResult
	if err := c.CallOp(r, ops.OpAccessMatrix, ops.AccessMatrixArgs{}, &res); err != nil {
		return nil, err
	}
	return accessPage(res), nil
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
