package web

import (
	"net/http"
	"regexp"
	"strconv"
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
		`<section class="panel" aria-labelledby="access-matrix-h" id="access-matrix">`,
		`id="access-refresh" class="btn btn-secondary" data-control="refresh" hx-get="/access" hx-target="#access-matrix" hx-select="#access-matrix" hx-swap="outerHTML" hx-disabled-elt="this"`, `<span class="refresh-idle">Refresh</span>`,
		`<button type="button" class="btn btn-secondary" data-access="print">Print</button>`, `<script src="/static/access.js" defer></script>`,
		`id="access-filter"`, `<b>C</b> Control`, `<b>★</b> via master`, `<b>⊘</b> blocked`, `<b>·</b> none`, `<b>◇</b> Claimed`, `<b>◆</b> Verified`,
		`class="access-scroll" tabindex="0" role="region" aria-label="`,
		// target Workers span their sessions; names are escaped
		`<th scope="colgroup" colspan="3" class="access-tw"><button type="submit" form="access-form" name="g" value="c:wB" class="access-toggle" aria-expanded="true" aria-label="Collapse ci">▾</button> <span class="access-name">ci</span> <span class="muted matrix-owner">&lt;b&gt;al&lt;/b&gt;</span></th>`,
		`aria-expanded="true" aria-label="Collapse lab">▾</button> <span class="access-name">lab</span> <span class="muted matrix-owner">root</span> <span class="muted">(offline)</span></th>`,
		`<th scope="col" class="access-ts"><span class="access-name">&lt;i&gt;x&lt;/i&gt;</span></th>`,
		// sources, grouped by Worker, with the proof chip
		`<th scope="rowgroup" colspan="1"><span class="access-name">boss-box</span> <span class="muted matrix-owner">root</span></th>`,
		`<th scope="row"><span class="access-name">boss</span> <span class="badge plain access-proof">◇ Claimed</span> <span class="muted">master</span></th>`,
		`<th scope="row"><span class="access-name">lead</span> <span class="badge plain access-proof">◆ Verified</span> <span class="muted">master</span></th>`,
		// glyphs
		`<td data-pair="wA|boss&gt;wB|b1" data-state="control/master" class="access-ok" title="Control of b1 on ci, via master"><a href="/access/why?source=wA%7Cboss&amp;target=wB%7Cb1">C★<span class="sr-only"> control via master</span></a></td>`,
		`data-state="blocked:not_shared" class="access-blocked" title="Blocked: the session is not shared"><a href="/access/why?source=wA%7Cboss&amp;target=wB%7C%3Ci%3Ex%3C%2Fi%3E">⊘<span class="sr-only"> blocked: the session is not shared</span></a></td>`,
		`<b>V</b> View`, `<b>M</b> Message`,
		`<a href="/access" aria-current="page">Who reaches what</a><a href="/access/grants">Grants</a><a href="/access/why">Why?</a></nav>`,
		`data-state="none" class="muted">·<span class="sr-only"> none</span></td>`,
		`title="same Worker: sessions of one Worker can already reach each other">=`,
		// the list form for a narrow container
		`<p id="access-status" role="status" class="access-status"></p>`, `<link rel="stylesheet" href="/static/access-matrix2.css">`,
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
		`data-state="view/grant" class="access-ok" title="View of b1 on ci, by a grant"><a href="/access/why?source=wC%7Chelper&amp;target=wB%7Cb1">V<span class="sr-only"> view by a grant</span></a></td>`,
		`<a href="/access/why?source=wC%7Chelper&amp;target=wB%7Cb2">M<span class="sr-only"> message by a grant</span></a>`,
		`target=wB%7C%3Ci%3Ex%3C%2Fi%3E">C<span class="sr-only"> control by a grant</span></a>`,
		`<th scope="row"><span class="access-name">helper</span> <span class="badge plain access-proof">◇ Claimed</span></th>`,
		// the hidden source: glyphs without a link
		`data-state="blocked:expired" class="access-blocked" title="Blocked: the grant has expired">⊘<span class="sr-only"> blocked: the grant has expired</span></td>`,
		`class="access-ok" title="View of b2 on ci, by a grant">V<span class="sr-only"> view by a grant</span></td>`)
	if strings.Count(body, "C★") != 3 || strings.Contains(body, "wD%7C") {
		t.Errorf("stars: %d, want 3; no link may name the hidden session", strings.Count(body, "C★"))
	}
}

// Only the Refresh button asks for the fragment: the section itself carries
// no hx-get, so a click into the filter or a cell starts no request.
func TestAccessPageRefreshOnlyFromTheButton(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, accessFixture()))
	body := get(t, adminServer(t), pathAccess, "X-Test-Session", userHeader).Body.String()
	if n := strings.Count(body, "hx-get="); n != 1 {
		t.Errorf("hx-get appears %d times, want 1 (the Refresh button)", n)
	}
	status, section := strings.Index(body, `role="status"`), strings.Index(body, `id="access-matrix"`)
	if status < 0 || section < status {
		t.Errorf("the status region (%d) must exist outside and before the swapped section (%d)", status, section)
	}
	js, err := staticFiles.ReadFile("static/access.js")
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, string(js), `"Updated, " + n + (n === 1 ? " change" : " changes")`, `classList.add("access-changed")`,
		`5 * 60 * 1000`, `access-stale`, `htmx:afterSwap`, `filter.value = typed`)
	mustNotHave(t, string(js), "localStorage", "sessionStorage", "indexedDB")
}

// The long form (shown under 700 px of container width) names every pair
// with its level or the blocking layer, grouped by source.
func TestAccessPageListNamesEveryPair(t *testing.T) {
	m := accessFixture()
	m.Sources = append(m.Sources, ops.AccessSource{WorkerID: "wC", WorkerName: "lab", Owner: "root", Session: "helper", Proof: ops.AccessProofClaimed},
		ops.AccessSource{WorkerID: "wD", WorkerName: "far", Owner: "bob", Session: "a session of bob", Proof: ops.AccessProofClaimed, Masked: true},
		ops.AccessSource{WorkerID: "wC", WorkerName: "lab", Owner: "root", Session: "idle", Proof: ops.AccessProofClaimed})
	m.Cells = append(m.Cells,
		ops.AccessCell{Source: 2, Target: 1, Granted: "message", InEffect: "message", Via: "g2"},
		ops.AccessCell{Source: 3, Target: 0, Granted: "view", Via: "g4", BlockedBy: ops.AccessBlockedExpired})
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, m))
	body := get(t, adminServer(t), pathAccess, "X-Test-Session", userHeader).Body.String()
	list := body[strings.Index(body, `<ul class="access-list">`):]
	mustHave(t, list,
		`<strong class="access-name">boss</strong> on <span class="access-name">boss-box</span> <span class="muted">root</span> ◇ Claimed master`,
		`<li data-pair="wA|boss&gt;wB|b1" data-state="control/master"><a href="/access/why?source=wA%7Cboss&amp;target=wB%7Cb1">boss on boss-box → b1 on ci: Control</a> <span class="muted">via master</span></li>`,
		`boss on boss-box → &lt;i&gt;x&lt;/i&gt; on ci: blocked, the session is not shared</a> <span class="muted">via master, granted Control</span></li>`,
		`lead on ci → c1 on lab: Control</a>`,
		`helper on lab → b2 on ci: Message</a> <span class="muted">by a grant</span></li>`,
		// a hidden session: the line without a link
		`data-state="blocked:expired">a session of bob on far → b1 on ci: blocked, the grant has expired <span class="muted">by a grant, granted View</span></li>`,
		`<strong class="access-name">idle</strong> on <span class="access-name">lab</span> <span class="muted">root</span> ◇ Claimed`,
		`<p class="muted access-pairs">reaches nothing now</p>`)
	if n := strings.Count(list, `<li data-pair=`); n != 6 {
		t.Errorf("the list has %d pairs, want 6 (one per cell)", n)
	}
	mustNotHave(t, body, "reaches: ")
}

// accessWide is one source on wA and n target sessions on each of two Workers.
func accessWide(n int) ops.AccessMatrixResult {
	m := ops.AccessMatrixResult{Sources: []ops.AccessSource{{WorkerID: "wA", WorkerName: "boss-box", Owner: "root", Session: "boss", Master: true}}}
	for _, w := range []string{"wB", "wC"} {
		for i := 0; i < n; i++ {
			m.Targets = append(m.Targets, ops.AccessTarget{WorkerID: w, WorkerName: "name-" + w, Owner: "root", Online: true, Session: "s" + strconv.Itoa(i)})
		}
	}
	for i := 0; i < n; i++ {
		m.Cells = append(m.Cells, ops.AccessCell{Source: 0, Target: i, Granted: "control", InEffect: "control", Via: "master"})
	}
	return m
}

func TestAccessPageCollapsesWorkerGroups(t *testing.T) {
	f := newFakeOps().reply(ops.OpAccessMatrix, accessFixture())
	connect(t, f)
	s := adminServer(t)
	body := get(t, s, pathAccess+"?g=c:wB", "X-Test-Session", userHeader).Body.String()
	mustHave(t, body,
		// one column with the aggregate, and the button that opens it again
		`<th scope="colgroup" colspan="1" class="access-tw"><button type="submit" form="access-form" name="g" value="o:wB" class="access-toggle" aria-expanded="false" aria-label="Expand ci">▸</button>`,
		`<th scope="col" class="access-ts access-first access-agg">(3)<span class="sr-only"> sessions of ci, collapsed</span></th>`,
		`<td data-agg="wA|boss&gt;wB|*" data-state="2/1/3" class="access-agg access-ok" title="2 of 3 sessions on ci in effect, 1 blocked">2/3<span class="sr-only"> 2 of 3 sessions on ci in effect, 1 blocked</span></td>`,
		// the state travels with the form and with Refresh and the export
		`<input type="hidden" name="g" value="c:wB">`, `hx-get="/access?g=c%3AwB"`, `href="/access.csv?g=c%3AwB"`,
		// the other group stays open; the list still names every pair
		`value="c:wC" class="access-toggle" aria-expanded="true" aria-label="Collapse lab"`, `boss on boss-box → b2 on ci: Control`)
	mustNotHave(t, body, `<span class="access-name">b1</span>`, `title="Control of b1 on ci, via master"`)
	// The last g for a Worker wins: the button adds its value after the hidden field.
	body = get(t, s, pathAccess+"?g=c:wB&g=o:wB", "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `value="c:wB" class="access-toggle" aria-expanded="true"`, `<input type="hidden" name="g" value="o:wB">`, `<span class="access-name">b1</span>`)
	if strings.Count(body, `name="g" value="o:wB"`) != 1 || strings.Contains(body, `<input type="hidden" name="g" value="c:wB">`) {
		t.Error("the state of a group is carried more than once")
	}

	// 40 session columns start open, 41 start collapsed; one group opens by its button.
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, accessWide(20)))
	body = get(t, adminServer(t), pathAccess, "X-Test-Session", userHeader).Body.String()
	if strings.Count(body, `aria-expanded="true"`) != 2 || strings.Contains(body, `aria-expanded="false"`) {
		t.Error("40 session columns: every group must start open")
	}
	m := accessWide(20)
	m.Targets = append(m.Targets, ops.AccessTarget{WorkerID: "wC", WorkerName: "name-wC", Owner: "root", Online: true, Session: "extra"})
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, m))
	s = adminServer(t)
	body = get(t, s, pathAccess, "X-Test-Session", userHeader).Body.String()
	if strings.Count(body, `aria-expanded="false"`) != 2 || strings.Contains(body, `aria-expanded="true"`) {
		t.Error("41 session columns: every group must start collapsed")
	}
	mustHave(t, body, `title="20 of 20 sessions on name-wB in effect">20/20<`, `title="0 of 21 sessions on name-wC in effect">0/21<`)
	body = get(t, s, pathAccess+"?g=o:wB", "X-Test-Session", userHeader).Body.String()
	if strings.Count(body, `aria-expanded="true"`) != 1 || strings.Count(body, `aria-expanded="false"`) != 1 || strings.Count(body, "C★") != 20 {
		t.Error("one opened group: its 20 cells are shown, the other stays collapsed")
	}
}

func TestAccessPageToolbar(t *testing.T) {
	m := accessFixture()
	m.Sources = append(m.Sources, ops.AccessSource{WorkerID: "wC", WorkerName: "lab", Owner: "root", Session: "idle", Proof: ops.AccessProofClaimed})
	m.Cells = append(m.Cells, ops.AccessCell{Source: 2, Target: 0, Granted: "view", Via: "g9", BlockedBy: ops.AccessBlockedSuspended})
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, m))
	s := adminServer(t)
	row := func(name string) string { return `<th scope="row"><span class="access-name">` + name + `</span>` }
	body := get(t, s, pathAccess, "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `<form id="access-form" method="get" action="/access" class="settings-tools access-tools access-form">`,
		`<input type="search" id="access-filter" name="q" value=""`,
		`<select name="owner"><option value="">all</option><option value="&lt;b&gt;al&lt;/b&gt;">&lt;b&gt;al&lt;/b&gt;</option><option value="root">root</option></select>`,
		`<input type="checkbox" name="access" value="1"> Only rows with access`,
		`<input type="radio" name="dir" value="who" checked> who → what`, `<input type="radio" name="dir" value="what"> what ← who`,
		`<button type="submit" class="btn btn-secondary access-apply">Apply</button>`, `href="/access.csv" download="access.csv">CSV</a>`,
		row("boss"), row("lead"), row("idle"))

	// Owner: the rows of that owner's Workers only.
	body = get(t, s, pathAccess+"?owner=root", "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `<option value="root" selected>root</option>`, row("boss"), row("idle"), `hx-get="/access?owner=root"`)
	mustNotHave(t, body, row("lead"), "lead on ci → c1 on lab")

	// Only rows with access: a source whose only pair is blocked goes.
	body = get(t, s, pathAccess+"?access=1", "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `name="access" value="1" checked>`, row("boss"), row("lead"))
	mustNotHave(t, body, row("idle"), "idle on lab →")

	// The search field filters on the server as well (no script needed).
	body = get(t, s, pathAccess+"?q=LEAD", "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `name="q" value="LEAD"`, row("lead"))
	mustNotHave(t, body, row("boss"))
	body = get(t, s, pathAccess+"?q=nobody", "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `No row matches the filters. <a href="/access">Show every row</a>`, `<form id="access-form"`)
	mustNotHave(t, body, `class="access-matrix"`)

	// what <- who: targets in rows, sources in columns.
	body = get(t, s, pathAccess+"?dir=what&owner=root", "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `<input type="radio" name="dir" value="what" checked>`, `Target ↓ &nbsp;·&nbsp; Source →`,
		`aria-label="Who reaches what: targets in rows, sources in columns"`,
		`aria-label="Collapse boss-box">▾</button> <span class="access-name">boss-box</span>`,
		`<th scope="col" class="access-ts access-first"><span class="access-name">boss</span> ◇<span class="sr-only"> Claimed</span></th>`,
		`<th scope="col" class="access-ts access-first"><span class="access-name">lead</span> ◆<span class="sr-only"> Verified</span></th>`,
		`<th scope="row"><span class="access-name">c1</span></th>`,
		`<td data-pair="wB|lead&gt;wC|c1" data-state="control/master" class="access-ok" title="Control for lead on ci, via master">`,
		`hx-get="/access?dir=what&amp;owner=root"`, `lead on ci → c1 on lab: Control`)
	// owner=root keeps the rows of lab only: no target of ci is a row
	mustNotHave(t, body, `<th scope="row"><span class="access-name">b1</span>`, "boss on boss-box → b1 on ci")
}

func TestAccessCSV(t *testing.T) {
	m := accessFixture()
	m.Targets[1].Session = "=b2"
	m.Sources = append(m.Sources, ops.AccessSource{WorkerID: "wC", WorkerName: "lab", Owner: "root", Session: "helper", Proof: ops.AccessProofClaimed})
	m.Cells = append(m.Cells, ops.AccessCell{Source: 2, Target: 0, Granted: "control", Via: "g7", BlockedBy: ops.AccessBlockedExpired})
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, m))
	s := adminServer(t)
	rec := get(t, s, pathAccessCSV, "X-Test-Session", userHeader)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/csv; charset=utf-8" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename="access.csv"` {
		t.Fatalf("GET %s = %d, %v", pathAccessCSV, rec.Code, rec.Header())
	}
	want := "source worker,source session,proof,target worker,target session,granted,in effect,via,blocked by\n" +
		"boss-box,boss,claimed,ci,b1,control,control,master,\n" +
		"boss-box,boss,claimed,ci,'=b2,control,control,master,\n" +
		"boss-box,boss,claimed,ci,<i>x</i>,control,,master,not_shared\n" +
		"ci,lead,verified,lab,c1,control,control,master,\n" +
		"lab,helper,claimed,ci,b1,control,,g7,expired\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("csv =\n%s\nwant\n%s", got, want)
	}
	// The filters of the page apply to the export.
	got := get(t, s, pathAccessCSV+"?owner=root&access=1", "X-Test-Session", userHeader).Body.String()
	if strings.Count(got, "\n") != 4 || strings.Contains(got, "lead") || strings.Contains(got, "helper") {
		t.Errorf("filtered csv =\n%s", got)
	}
	got = get(t, s, pathAccessCSV+"?dir=what&q=c1", "X-Test-Session", userHeader).Body.String()
	if strings.Count(got, "\n") != 2 || !strings.Contains(got, "ci,lead,verified,lab,c1,control,control,master,\n") {
		t.Errorf("csv by target =\n%s", got)
	}
	if rec := get(t, s, pathAccessCSV); rec.Code == http.StatusOK {
		t.Error("the export answers without a session")
	}
}

// Names are never cut: the stylesheet of the matrix wraps them and uses the
// tokens of minion.css only.
func TestAccessMatrixStylesWrapNames(t *testing.T) {
	css, err := staticFiles.ReadFile("static/access-matrix2.css")
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, string(css), `.access-name { display: inline-block; width: max-content; max-width: 28ch; white-space: normal; overflow-wrap: anywhere;`,
		`.matrix-tools .matrix-asof.access-stale { color: var(--warning);`, `#access-matrix .access-changed {`)
	mustNotHave(t, string(css), "ellipsis", "nowrap", "rgb(", "hsl(")
	if regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`).MatchString(string(css)) {
		t.Error("access-matrix2.css names a colour outside the tokens")
	}
	old, err := staticFiles.ReadFile("static/minion.css")
	if err != nil {
		t.Fatal(err)
	}
	start, end := strings.Index(string(old), "/* access: matrix */"), strings.Index(string(old), "/* end access: matrix */")
	if start < 0 || end < start || strings.Contains(string(old)[start:end], "ellipsis") {
		t.Error("the matrix block of minion.css cuts a name")
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
