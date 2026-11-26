package web

// Access, "Why?": the layered explainer of one pair (DESIGN 11.4). The page
// asks access_explain and shows every check in order, with the first failing
// one as "Blocked by". It is a plain GET form: it works without scripts, and
// a link with ?source=...&target=... opens the answer directly.
//
// What the page may reveal is decided here in one place: a pair the viewer
// may not see in full (the operation refuses it, or masks one end) gets the
// same neutral answer as a pair that does not exist, and no name is shown
// that the operation did not return.

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const pathAccessWhy = "/access/why"

// accessWhyNeutral is the one answer for every pair without an explanation:
// unknown Worker, unknown session, or a session the viewer may not see.
const accessWhyNeutral = "There is no explanation for this pair. Either it does not exist, or you may not see one of its sessions."

// AccessWhyPage is what access_why.html sees as .Data.
type AccessWhyPage struct {
	Workers        []AccessWhyGroup
	Source, Target string // the selected "<worker id>|<session>"
	// Asked: both ends were chosen. Neutral is set when the pair has no
	// answer the viewer may see; otherwise Question, Summary and Checks are.
	Asked    bool
	Neutral  string
	Question string
	Summary  string
	Reaches  bool
	// Blocked is the first failing check ("Blocked by"), nil when none fails.
	Blocked *AccessWhyCheck
	Checks  []AccessWhyCheck
}

// AccessWhyGroup is one Worker and its sessions in the two selects. Label is
// the Worker's name, with its owner when two Workers share a name.
type AccessWhyGroup struct {
	ID, Label, Name, Owner string
	Sessions               []AccessWhyOption
}

// AccessWhyOption is one session; Value is "<worker id>|<session>". A Worker
// id never holds "|", so the first "|" always ends it, whatever the session
// name holds.
type AccessWhyOption struct{ Value, Session string }

// AccessWhyCheck is one layer of the answer.
type AccessWhyCheck struct {
	ID, State, Text string
	// Code is the stable reason code, when the operation reports one.
	Code string
	// FixHref and FixLabel are the link that repairs a failing check, when
	// the operation gives one.
	FixHref, FixLabel string
	// Blocking marks the first failing check.
	Blocking bool
}

// StateClass is the state as a class suffix: one of the four known states.
func (c AccessWhyCheck) StateClass() string {
	switch c.State {
	case ops.AccessCheckPass, ops.AccessCheckFail, ops.AccessCheckSkip:
		return c.State
	}
	return ops.AccessCheckUnknown
}

// Mark says the state in a sign and a word (never by colour alone).
func (c AccessWhyCheck) Mark() string { return accessCheckMark(c.State) }

// Label is the name of the layer.
func (c AccessWhyCheck) Label() string {
	switch c.ID {
	case ops.AccessCheckCaller:
		return "Caller"
	case ops.AccessCheckGrant:
		return "Grant"
	case ops.AccessCheckAuthority:
		return "Authority"
	case ops.AccessCheckTime:
		return "Time"
	case ops.AccessCheckSharing:
		return "Sharing"
	case ops.AccessCheckWorkers:
		return "Workers"
	case ops.AccessCheckPreviewOnly:
		return "Preview-only"
	}
	return c.ID
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

// accessWhyAnswer is access_explain as this page reads it: the result of the
// operation plus the optional members "code" and "fix" of a check, which a
// Hub may or may not send.
type accessWhyAnswer struct {
	Source   ops.AccessSource `json:"source"`
	Target   ops.AccessTarget `json:"target"`
	InEffect string           `json:"in_effect"`
	Via      string           `json:"via"`
	Checks   []struct {
		ID    string          `json:"id"`
		State string          `json:"state"`
		Text  string          `json:"text"`
		Code  string          `json:"code"`
		Fix   json.RawMessage `json:"fix"`
	} `json:"checks"`
}

const accessWhyFixLabel = "Fix this"

// accessWhyFix reads the member "fix" of a check: a path, or an object with
// "href" (or "url") and "label" (or "text"). Only a path of this Hub is a
// link; anything else is dropped.
func accessWhyFix(raw json.RawMessage) (href, label string) {
	if len(raw) == 0 {
		return "", ""
	}
	var o struct{ Href, URL, Label, Text string }
	if json.Unmarshal(raw, &href) != nil {
		if json.Unmarshal(raw, &o) != nil {
			return "", ""
		}
		href, label = o.Href, o.Label
		if href == "" {
			href = o.URL
		}
		if label == "" {
			label = o.Text
		}
	}
	if !strings.HasPrefix(href, "/") || strings.HasPrefix(href, "//") || strings.ContainsAny(href, "\\\r\n\t") {
		return "", ""
	}
	if label = strings.TrimSpace(label); label == "" {
		label = accessWhyFixLabel
	}
	return href, label
}

// minionControlLink reports whether href leads to Minion control, a page for
// administrators only.
func minionControlLink(href string) bool {
	rest, ok := strings.CutPrefix(href, pathMinionControl)
	return ok && (rest == "" || strings.ContainsRune("/?#", rune(rest[0])))
}

func accessWhyVerb(level string) string {
	switch level {
	case ops.AccessLevelControl:
		return "type into"
	case ops.AccessLevelMessage:
		return "send messages to"
	case ops.AccessLevelView:
		return "view"
	}
	return ""
}

// accessWhySummary is the one sentence above the checks.
func accessWhySummary(res ops.AccessExplainResult) string {
	src, tg := res.Source.Session+" on "+res.Source.WorkerName, res.Target.Session+" on "+res.Target.WorkerName
	if verb := accessWhyVerb(res.InEffect); verb != "" {
		return src + " can " + verb + " " + tg + "."
	}
	for _, c := range res.Checks {
		if c.State == ops.AccessCheckFail {
			return src + " cannot reach " + tg + ": " + c.Text + "."
		}
	}
	return src + " cannot reach " + tg + "."
}

// accessWhyQuestion is the heading of the answer, the question it answers.
func accessWhyQuestion(res ops.AccessExplainResult) string {
	src, tg := res.Source.Session+" on "+res.Source.WorkerName, res.Target.Session+" on "+res.Target.WorkerName
	if verb := accessWhyVerb(res.InEffect); verb != "" {
		return "Why can " + src + " " + verb + " " + tg + "?"
	}
	return "Why can " + src + " not reach " + tg + "?"
}

// accessWhyOptions collects the sessions of the two selects by Worker id.
type accessWhyOptions struct {
	groups []AccessWhyGroup
	seen   map[string]bool
}

func (o *accessWhyOptions) add(workerID, worker, owner, session string) {
	if workerID == "" || session == "" || strings.Contains(workerID, "|") {
		return
	}
	v := workerID + "|" + session
	if o.seen == nil {
		o.seen = map[string]bool{}
	}
	if o.seen[v] {
		return
	}
	o.seen[v] = true
	for i := range o.groups {
		if o.groups[i].ID == workerID {
			o.groups[i].Sessions = append(o.groups[i].Sessions, AccessWhyOption{v, session})
			return
		}
	}
	o.groups = append(o.groups, AccessWhyGroup{ID: workerID, Name: worker, Owner: owner, Sessions: []AccessWhyOption{{v, session}}})
}

// sorted returns the groups by Worker name and their sessions in natural
// order. Two Workers of one name are told apart by their owner.
func (o *accessWhyOptions) sorted() []AccessWhyGroup {
	names := map[string]int{}
	for _, g := range o.groups {
		names[g.Name]++
	}
	for i := range o.groups {
		g := &o.groups[i]
		g.Label = g.Name
		if names[g.Name] > 1 && g.Owner != "" {
			g.Label = g.Name + " (" + g.Owner + ")"
		}
		slices.SortFunc(g.Sessions, func(a, b AccessWhyOption) int { return ops.NaturalCompare(a.Session, b.Session) })
	}
	slices.SortStableFunc(o.groups, func(a, b AccessWhyGroup) int {
		if c := ops.NaturalCompare(a.Label, b.Label); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return o.groups
}

// accessWhyRefused tells whether a failure of access_explain is the Hub's
// "no answer for this pair" (unknown, not yours, or not a valid pair), which
// the page answers neutrally. Anything else is a failure of the page.
func accessWhyRefused(err error) bool {
	var oe *OpError
	return errors.As(err, &oe) && oe.Status >= 400 && oe.Status < 500 &&
		oe.Status != http.StatusUnauthorized && oe.Status != http.StatusTooManyRequests
}

func accessWhyData(r *http.Request, c OpCaller, u pageUser) (any, error) {
	var m ops.AccessMatrixResult
	if err := c.CallOp(r, ops.OpAccessMatrix, ops.AccessMatrixArgs{}, &m); err != nil {
		return nil, err
	}
	p := AccessWhyPage{Source: r.URL.Query().Get("source"), Target: r.URL.Query().Get("target")}
	var opts accessWhyOptions
	// The live sessions the viewer may see (the snapshot of the Grants page)
	// come first: a pair without any grant is the one most often asked about.
	_, live := grantWorkers(r, u)
	for _, w := range live {
		for _, s := range w.Sessions {
			opts.add(w.ID, w.Name, w.Owner, s)
		}
	}
	// Then the ends of the matrix: sessions with reach that are not live now.
	for _, s := range m.Sources {
		if !s.Masked {
			opts.add(s.WorkerID, s.WorkerName, s.Owner, s.Session)
		}
	}
	for _, t := range m.Targets {
		if !t.Masked {
			opts.add(t.WorkerID, t.WorkerName, t.Owner, t.Session)
		}
	}
	sw, ss, okS := strings.Cut(p.Source, "|")
	tw, ts, okT := strings.Cut(p.Target, "|")
	if !okS || !okT || sw == "" || ss == "" || tw == "" || ts == "" {
		p.Workers = opts.sorted()
		return p, nil
	}
	p.Asked = true
	var res accessWhyAnswer
	if err := c.CallOp(r, ops.OpAccessExplain, ops.AccessExplainArgs{SourceWorkerID: sw, SourceSession: ss, TargetWorkerID: tw, TargetSession: ts}, &res); err != nil {
		if !accessWhyRefused(err) {
			return nil, err
		}
		p.Neutral = accessWhyNeutral
		p.Workers = opts.sorted()
		return p, nil
	}
	// One end is hidden from this viewer, or the answer is for another pair
	// than the one asked: the checks would tell whether the hidden session
	// exists, so the answer is the neutral one.
	if res.Source.Masked || res.Target.Masked ||
		res.Source.WorkerID != sw || res.Source.Session != ss || res.Target.WorkerID != tw || res.Target.Session != ts {
		p.Neutral = accessWhyNeutral
		p.Workers = opts.sorted()
		return p, nil
	}
	// A pair that came by link and is not in the lists is still selectable,
	// under the names the operation returned.
	opts.add(res.Source.WorkerID, res.Source.WorkerName, res.Source.Owner, res.Source.Session)
	opts.add(res.Target.WorkerID, res.Target.WorkerName, res.Target.Owner, res.Target.Session)
	plain := ops.AccessExplainResult{Source: res.Source, Target: res.Target, InEffect: res.InEffect, Via: res.Via}
	for _, ch := range res.Checks {
		plain.Checks = append(plain.Checks, ops.AccessCheck{ID: ch.ID, State: ch.State, Text: ch.Text})
		v := AccessWhyCheck{ID: ch.ID, State: ch.State, Text: ch.Text, Code: ch.Code}
		v.FixHref, v.FixLabel = accessWhyFix(ch.Fix)
		if !u.Admin && minionControlLink(v.FixHref) {
			// Minion control is for administrators (404 for anyone else): the
			// check keeps its text, without a link that cannot work.
			v.FixHref, v.FixLabel = "", ""
		}
		p.Checks = append(p.Checks, v)
	}
	for i := range p.Checks {
		if p.Checks[i].State == ops.AccessCheckFail {
			p.Checks[i].Blocking = true
			p.Blocked = &p.Checks[i]
			break
		}
	}
	p.Reaches = accessWhyVerb(res.InEffect) != ""
	p.Question, p.Summary = accessWhyQuestion(plain), accessWhySummary(plain)
	p.Workers = opts.sorted()
	return p, nil
}

func init() {
	RegisterPage(Page{Pattern: pathAccessWhy, Title: "Access", Template: "access_why.html", Data: userPageData(accessWhyData)})
}
