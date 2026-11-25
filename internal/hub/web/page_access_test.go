package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// accessFixture: boss on boss-box reaches two sessions on ci, is blocked on
// a third, and has no access to lab; lead on ci reaches lab and sees its own
// Worker's columns as "same Worker".
func accessFixture() ops.AccessMatrixResult {
	return ops.AccessMatrixResult{
		AsOf: ops.Timestamp(time.Date(2026, 11, 25, 14, 2, 11, 0, time.UTC)),
		Sources: []ops.AccessSource{
			{WorkerID: "wA", WorkerName: "boss-box", Owner: "root", Session: "boss", Proof: ops.AccessProofClaimed, Master: true},
			{WorkerID: "wB", WorkerName: "ci", Owner: "<b>al</b>", Session: "lead", Proof: ops.AccessProofVerified, Master: true},
		},
		Targets: []ops.AccessTarget{
			{WorkerID: "wB", WorkerName: "ci", Owner: "<b>al</b>", Online: true, Session: "b1"},
			{WorkerID: "wB", WorkerName: "ci", Owner: "<b>al</b>", Online: true, Session: "b2"},
			{WorkerID: "wB", WorkerName: "ci", Owner: "<b>al</b>", Online: true, Session: "<i>x</i>"},
			{WorkerID: "wC", WorkerName: "lab", Owner: "root", Session: "c1"},
		},
		Cells: []ops.AccessCell{
			{Source: 0, Target: 0, Granted: "control", InEffect: "control", Via: "master"},
			{Source: 0, Target: 1, Granted: "control", InEffect: "control", Via: "master"},
			{Source: 0, Target: 2, Granted: "control", Via: "master", BlockedBy: ops.AccessBlockedNotShared},
			{Source: 1, Target: 3, Granted: "control", InEffect: "control", Via: "master"},
		},
	}
}

func TestAccessPageShowsWhoReachesWhat(t *testing.T) {
	f := newFakeOps().reply(ops.OpAccessMatrix, accessFixture())
	connect(t, f)
	s := adminServer(t)
	rec := get(t, s, pathAccess, "X-Test-Session", userHeader)
	if rec.Code != http.StatusOK {
		t.Fatalf("a signed-in user GET %s = %d", pathAccess, rec.Code)
	}
	body := rec.Body.String()
	mustHave(t, body, `<title>Access`, `class="page-head access-wide"`, `<h1>Who reaches what</h1>`,
		`href="/access"`,
		`As of <time datetime="2026-11-25T14:02:11Z">`,
		`id="access-matrix" hx-get="/access" hx-select="#access-matrix" hx-swap="outerHTML"`,
		`data-control="refresh" hx-get="/access" hx-target="#access-matrix" hx-disabled-elt="this"`, `<span class="refresh-idle">Refresh</span>`,
		`<button type="button" class="btn btn-secondary" data-access="print">Print</button>`, `<script src="/static/access.js" defer></script>`,
		`id="access-filter"`, `<b>C</b> Control`, `<b>★</b> via master`, `<b>⊘</b> blocked`, `<b>·</b> none`, `<b>◇</b> Claimed`, `<b>◆</b> Verified`,
		`class="access-scroll" tabindex="0" role="region" aria-label="`,
		// target Workers span their sessions; names are escaped
		`<th scope="colgroup" colspan="3" class="access-tw">ci <span class="muted matrix-owner">&lt;b&gt;al&lt;/b&gt;</span></th>`,
		`<th scope="colgroup" colspan="1" class="access-tw">lab <span class="muted matrix-owner">root</span> <span class="muted">(offline)</span></th>`,
		`<th scope="col" class="access-ts">&lt;i&gt;x&lt;/i&gt;</th>`,
		// sources, grouped by Worker, with the proof chip
		`<th scope="rowgroup" colspan="1">boss-box <span class="muted matrix-owner">root</span></th>`,
		`<th scope="row">boss <span class="badge plain access-proof">◇ Claimed</span> <span class="muted">master</span></th>`,
		`<th scope="row">lead <span class="badge plain access-proof">◆ Verified</span> <span class="muted">master</span></th>`,
		// glyphs
		`<td class="access-ok" title="Control of b1 on ci, via master"><a href="/access/why?source=wA%7Cboss&amp;target=wB%7Cb1">C★<span class="sr-only"> control via master</span></a></td>`,
		`<td class="access-blocked" title="Blocked: the session is not shared"><a href="/access/why?source=wA%7Cboss&amp;target=wB%7C%3Ci%3Ex%3C%2Fi%3E">⊘<span class="sr-only"> blocked: the session is not shared</span></a></td>`,
		`<b>V</b> View`, `<b>M</b> Message`,
		`<a href="/access" aria-current="page">Who reaches what</a><a href="/access/grants">Grants</a><a href="/access/why">Why?</a></nav>`,
		`<td class="muted">·<span class="sr-only"> none</span></td>`,
		`title="same Worker: sessions of one Worker can already reach each other">=`,
		// the list form for a narrow container
		`reaches: 2 sessions on ci`, `reaches: 1 session on lab`,
		`Sessions on the same Worker are not governed here: every session of a Worker can already reach the others on that host.`)
	if strings.Contains(body, "<i>x</i>") || strings.Contains(body, "<b>al</b>") {
		t.Error("a name is not escaped")
	}
	if strings.Count(body, "C★") != 3 || strings.Count(body, `class="access-blocked"`) != 1 {
		t.Errorf("glyphs: %d in effect, %d blocked; want 3 and 1", strings.Count(body, "C★"), strings.Count(body, `class="access-blocked"`))
	}
	// Navigation: Access sits in the Access group after API keys and before Minion control.
	admin := get(t, s, pathAccess, "X-Test-Session", adminHeader).Body.String()
	keys, access, control := strings.Index(admin, `href="/api-keys"`), strings.Index(admin, `href="/access"`), strings.Index(admin, `href="/access/control"`)
	if keys < 0 || access < keys || control < access {
		t.Errorf("navigation order: API keys %d, Access %d, Minion control %d", keys, access, control)
	}
	css, err := staticFiles.ReadFile("static/minion.css")
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, string(css), "/* access: matrix */", "/* end access: matrix */", `.access-box { container-type: inline-size; }`, `@container (max-width: 699px) {`)
	if start, end := strings.Index(string(css), "/* access: matrix */"), strings.Index(string(css), "/* end access: matrix */"); start < 0 || end < start {
		t.Error("the access matrix block of minion.css is not delimited")
	}
	js, err := staticFiles.ReadFile("static/access.js")
	if err != nil || !strings.Contains(string(js), "window.print()") {
		t.Errorf("access.js: %v", err)
	}
}

func TestAccessPageEmptyState(t *testing.T) {
	f := newFakeOps().reply(ops.OpAccessMatrix, ops.AccessMatrixResult{Sources: []ops.AccessSource{}, Targets: []ops.AccessTarget{}, Cells: []ops.AccessCell{}})
	connect(t, f)
	body := get(t, adminServer(t), pathAccess, "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `No session reaches another Worker yet.`, `<a href="/access/control">Minion control</a>`, `<span class="refresh-idle">Refresh</span>`)
	if strings.Contains(body, `class="access-matrix"`) {
		t.Error("an empty matrix renders a table")
	}
}

// Grant cells show the level without a star; a cell whose session name is
// hidden (another owner's session) is not a link.
func TestAccessPageGrantCells(t *testing.T) {
	m := accessFixture()
	m.Sources = append(m.Sources, ops.AccessSource{WorkerID: "wC", WorkerName: "lab", Owner: "root", Session: "helper", Proof: ops.AccessProofClaimed},
		ops.AccessSource{WorkerID: "wD", WorkerName: "far", Owner: "bob", Session: "a session of bob", Proof: ops.AccessProofClaimed, Masked: true})
	m.Cells = append(m.Cells,
		ops.AccessCell{Source: 2, Target: 0, Granted: "view", InEffect: "view", Via: "g1"},
		ops.AccessCell{Source: 2, Target: 1, Granted: "message", InEffect: "message", Via: "g2"},
		ops.AccessCell{Source: 2, Target: 2, Granted: "control", InEffect: "control", Via: "g3"},
		ops.AccessCell{Source: 3, Target: 0, Granted: "control", Via: "g4", BlockedBy: ops.AccessBlockedExpired},
		ops.AccessCell{Source: 3, Target: 1, Granted: "view", InEffect: "view", Via: "g5"})
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, m))
	body := get(t, adminServer(t), pathAccess, "X-Test-Session", userHeader).Body.String()
	mustHave(t, body,
		`<td class="access-ok" title="View of b1 on ci, by a grant"><a href="/access/why?source=wC%7Chelper&amp;target=wB%7Cb1">V<span class="sr-only"> view by a grant</span></a></td>`,
		`<a href="/access/why?source=wC%7Chelper&amp;target=wB%7Cb2">M<span class="sr-only"> message by a grant</span></a>`,
		`target=wB%7C%3Ci%3Ex%3C%2Fi%3E">C<span class="sr-only"> control by a grant</span></a>`,
		`<th scope="row">helper <span class="badge plain access-proof">◇ Claimed</span></th>`,
		// the hidden source: glyphs without a link
		`<td class="access-blocked" title="Blocked: the grant has expired">⊘<span class="sr-only"> blocked: the grant has expired</span></td>`,
		`<td class="access-ok" title="View of b2 on ci, by a grant">V<span class="sr-only"> view by a grant</span></td>`)
	if strings.Count(body, "C★") != 3 || strings.Contains(body, "wD%7C") {
		t.Errorf("stars: %d, want 3; no link may name the hidden session", strings.Count(body, "C★"))
	}
}

func TestAccessWhyPage(t *testing.T) {
	explain := ops.AccessExplainResult{
		Source: ops.AccessSource{WorkerID: "wA", WorkerName: "boss-box", Owner: "root", Session: "boss", Proof: ops.AccessProofClaimed, Master: true},
		Target: ops.AccessTarget{WorkerID: "wB", WorkerName: "ci", Owner: "al", Online: true, Session: "<i>x</i>"},
		Via:    "master",
		Checks: []ops.AccessCheck{
			{ID: ops.AccessCheckCaller, State: ops.AccessCheckPass, Text: "the source session is named by its Worker (Claimed)"},
			{ID: ops.AccessCheckGrant, State: ops.AccessCheckPass, Text: "master reach gives Control"},
			{ID: ops.AccessCheckAuthority, State: ops.AccessCheckPass, Text: "ok"},
			{ID: ops.AccessCheckTime, State: ops.AccessCheckPass, Text: "ok"},
			{ID: ops.AccessCheckSharing, State: ops.AccessCheckFail, Text: "the session is not shared by its Worker"},
			{ID: ops.AccessCheckWorkers, State: ops.AccessCheckSkip, Text: "not evaluated: an earlier check failed"},
			{ID: ops.AccessCheckPreviewOnly, State: ops.AccessCheckUnknown, Text: "the target Worker decides this and does not report it"},
		},
	}
	f := newFakeOps().reply(ops.OpAccessMatrix, accessFixture()).reply(ops.OpAccessExplain, explain)
	connect(t, f)
	s := adminServer(t)
	// Without a pair: the form only.
	body := get(t, s, pathAccessWhy, "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `<title>Access`, `<h1>Why?</h1>`,
		`<a href="/access">Who reaches what</a><a href="/access/grants">Grants</a><a href="/access/why" aria-current="page">Why?</a></nav>`,
		`<form method="get" action="/access/why"`, `<select name="source" required>`, `<select name="target" required>`,
		`<optgroup label="boss-box"><option value="wA|boss">boss</option></optgroup>`,
		`<option value="wB|&lt;i&gt;x&lt;/i&gt;">&lt;i&gt;x&lt;/i&gt;</option>`, `<button type="submit" class="btn">Check</button>`)
	if strings.Contains(body, "access-why-checks") || strings.Contains(body, "onchange") {
		t.Error("the page answers (or submits by itself) before a pair is chosen")
	}
	rec := get(t, s, pathAccessWhy+"?source=wA%7Cboss&target=wB%7C%3Ci%3Ex%3C%2Fi%3E", "X-Test-Session", userHeader)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET why = %d", rec.Code)
	}
	body = rec.Body.String()
	mustHave(t, body, `<option value="wA|boss" selected>boss</option>`, `<option value="wB|&lt;i&gt;x&lt;/i&gt;" selected>`,
		`<strong>boss on boss-box cannot reach &lt;i&gt;x&lt;/i&gt; on ci: the session is not shared by its Worker.</strong>`,
		`<ol class="access-why-checks"><li class="access-why-pass"><span class="access-why-mark">✔ pass</span> <span class="access-why-id">caller</span>`,
		`<li class="access-why-fail"><span class="access-why-mark">✘ fail</span> <span class="access-why-id">sharing</span> <span class="access-why-text">the session is not shared by its Worker</span></li>`,
		`<span class="access-why-mark">– not evaluated</span> <span class="access-why-id">workers</span>`,
		`<span class="access-why-mark">? unknown</span> <span class="access-why-id">preview_only</span> <span class="access-why-text">the target Worker decides this and does not report it</span>`)
	if strings.Contains(body, "<i>x</i>") {
		t.Error("a name is not escaped")
	}
	if strings.Count(body, `href="/access/why"`) != 1 {
		t.Error("the why page has a navigation entry of its own")
	}
	explain.InEffect = ops.AccessLevelControl
	if got := accessWhySummary(explain); got != "boss on boss-box can type into <i>x</i> on ci." {
		t.Errorf("summary = %q", got)
	}
	css, _ := staticFiles.ReadFile("static/minion.css")
	mustHave(t, string(css), "/* access: why */", "/* end access: why */")
}
