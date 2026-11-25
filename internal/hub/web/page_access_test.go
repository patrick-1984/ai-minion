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
		`title="Control of b1 on ci, via master">C★<span class="sr-only"> control via master</span></td>`,
		`<td class="access-blocked" title="Blocked: the session is not shared">⊘<span class="sr-only"> blocked: the session is not shared</span></td>`,
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
	if !strings.HasSuffix(strings.TrimSpace(string(css)), "/* end access: matrix */") {
		t.Error("the access block is not the end of minion.css")
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
