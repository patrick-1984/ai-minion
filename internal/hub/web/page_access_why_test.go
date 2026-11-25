package web

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func whyExplainFixture() ops.AccessExplainResult {
	return ops.AccessExplainResult{
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
}

const whyPairQuery = "?source=wA%7Cboss&target=wB%7C%3Ci%3Ex%3C%2Fi%3E"

func TestAccessWhyPage(t *testing.T) {
	explain := whyExplainFixture()
	f := newFakeOps().reply(ops.OpAccessMatrix, accessFixture()).reply(ops.OpAccessExplain, explain)
	connect(t, f)
	s := adminServer(t)
	// Without a pair: the form only.
	body := get(t, s, pathAccessWhy, "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `<title>Access`, `<h1>Why?</h1>`, `<link rel="stylesheet" href="/static/access-why.css">`,
		`<a href="/access">Who reaches what</a><a href="/access/grants">Grants</a><a href="/access/why" aria-current="page">Why?</a></nav>`,
		`<form method="get" action="/access/why"`, `<select name="source" required>`, `<select name="target" required>`,
		`<optgroup label="boss-box"><option value="wA|boss">boss</option></optgroup>`,
		`<option value="wB|&lt;i&gt;x&lt;/i&gt;">&lt;i&gt;x&lt;/i&gt;</option>`, `<button type="submit" class="btn">Check</button>`)
	mustNotHave(t, body, "access-why-checks", "access-why-answer", "onchange", "autofocus")
	if len(f.called(ops.OpAccessExplain)) != 0 {
		t.Error("the page asks for an explanation before a pair is chosen")
	}

	// A deep link answers at once: the question (focused), the summary, the
	// first failing check as "Blocked by", every check in order with a mark.
	rec := get(t, s, pathAccessWhy+whyPairQuery, "X-Test-Session", userHeader)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET why = %d", rec.Code)
	}
	body = rec.Body.String()
	mustHave(t, body, `<option value="wA|boss" selected>boss</option>`, `<option value="wB|&lt;i&gt;x&lt;/i&gt;" selected>`,
		`<div class="access-why-answer" data-answer="blocked">`,
		`<h3 id="access-why-answer" class="access-why-question" tabindex="-1" autofocus>Why can boss on boss-box not reach &lt;i&gt;x&lt;/i&gt; on ci?</h3>`,
		`<p class="access-why-summary" role="status"><strong>boss on boss-box cannot reach &lt;i&gt;x&lt;/i&gt; on ci: the session is not shared by its Worker.</strong></p>`,
		`<p class="access-why-blocked"><span class="badge" data-tone="danger">Blocked by</span> <span>Sharing: the session is not shared by its Worker</span></p>`,
		`<ol class="access-why-checks"><li class="access-why-pass" data-check="caller" data-state="pass"><span class="access-why-mark">✔ pass</span> <span class="access-why-id">Caller</span>`,
		`<li class="access-why-fail access-why-blocking" data-check="sharing" data-state="fail"><span class="access-why-mark">✘ fail</span> <span class="access-why-id">Sharing</span> <span class="access-why-text">the session is not shared by its Worker</span> <span class="access-why-first">Blocked by this check</span></li>`,
		`<li class="access-why-skip" data-check="workers" data-state="skip"><span class="access-why-mark">– not evaluated</span> <span class="access-why-id">Workers</span>`,
		`<span class="access-why-mark">? unknown</span> <span class="access-why-id">Preview-only</span> <span class="access-why-text">the target Worker decides this and does not report it</span></li></ol>`)
	mustNotHave(t, body, "<i>x</i>", "data-code", "access-why-fix", "<script>", ` style="`)
	if strings.Count(body, "access-why-blocking") != 1 || strings.Count(body, "autofocus") != 1 {
		t.Error("exactly one check is the blocking one and exactly one element takes the focus")
	}
	if strings.Count(body, `href="/access/why"`) != 1 {
		t.Error("the why page has a navigation entry of its own")
	}
	// The checks keep the order of the operation.
	last := -1
	for _, id := range []string{"caller", "grant", "authority", "time", "sharing", "workers", "preview_only"} {
		i := strings.Index(body, `data-check="`+id+`"`)
		if i <= last {
			t.Fatalf("check %s is out of order", id)
		}
		last = i
	}

	explain.InEffect = ops.AccessLevelControl
	if got := accessWhySummary(explain); got != "boss on boss-box can type into <i>x</i> on ci." {
		t.Errorf("summary = %q", got)
	}
	if got := accessWhyQuestion(explain); got != "Why can boss on boss-box type into <i>x</i> on ci?" {
		t.Errorf("question = %q", got)
	}
	css, err := staticFiles.ReadFile("static/access-why.css")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`).Match(css) {
		t.Error("access-why.css holds a hex colour; use the tokens of minion.css")
	}
}

// A pair that reaches: no "Blocked by", the answer says so.
func TestAccessWhyPageOfAPairThatReaches(t *testing.T) {
	explain := whyExplainFixture()
	explain.InEffect = ops.AccessLevelMessage
	for i := range explain.Checks {
		if explain.Checks[i].State != ops.AccessCheckUnknown {
			explain.Checks[i].State = ops.AccessCheckPass
		}
	}
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, accessFixture()).reply(ops.OpAccessExplain, explain))
	body := get(t, adminServer(t), pathAccessWhy+whyPairQuery, "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `data-answer="reaches"`, `autofocus>Why can boss on boss-box send messages to &lt;i&gt;x&lt;/i&gt; on ci?</h3>`,
		`<strong>boss on boss-box can send messages to &lt;i&gt;x&lt;/i&gt; on ci.</strong>`)
	mustNotHave(t, body, "Blocked by", "access-why-blocking")
}

// The finding "Why reveals whether a private session exists": whatever the
// operation says about a pair with a hidden end, the page is the same as for
// a pair that does not exist, and it never shows the name that was asked.
func TestAccessWhyGivesOneNeutralAnswerForHiddenAndAbsentPairs(t *testing.T) {
	const query = "?source=wA%7Cboss&target=wB%7Csecret-plan"
	masked := func(grantText string) ops.AccessExplainResult {
		return ops.AccessExplainResult{
			Source: ops.AccessSource{WorkerID: "wA", WorkerName: "boss-box", Owner: "root", Session: "boss"},
			Target: ops.AccessTarget{WorkerID: "wB", WorkerName: "ci", Owner: "al", Session: "a session of al", Masked: true},
			Checks: []ops.AccessCheck{
				{ID: ops.AccessCheckCaller, State: ops.AccessCheckPass, Text: "ok"},
				{ID: ops.AccessCheckGrant, State: ops.AccessCheckFail, Text: grantText},
			},
		}
	}
	echoed := whyExplainFixture() // an answer for another pair than the one asked
	replies := map[string]func(map[string]any) (any, error){
		"not found": func(map[string]any) (any, error) {
			return nil, &OpError{Status: http.StatusNotFound, Code: "not_found", Message: "no Worker wB or session secret-plan"}
		},
		"invalid": func(map[string]any) (any, error) {
			return nil, &OpError{Status: http.StatusBadRequest, Code: "invalid_argument", Message: "secret-plan"}
		},
		"hidden, exists": func(map[string]any) (any, error) {
			return masked("no grant and no master reach covers this pair"), nil
		},
		"hidden, absent": func(map[string]any) (any, error) {
			return masked("no grant and no master reach covers this pair: the target Worker does not list a session of that name now"), nil
		},
		"hidden source": func(map[string]any) (any, error) {
			r := masked("x")
			r.Target.Masked, r.Target.Session, r.Source.Masked = false, "secret-plan", true
			return r, nil
		},
		"another pair": func(map[string]any) (any, error) { return echoed, nil },
	}
	bodies := map[string]string{}
	for name, fn := range replies {
		connect(t, newFakeOps().reply(ops.OpAccessMatrix, accessFixture()).on(ops.OpAccessExplain, fn))
		rec := get(t, adminServer(t), pathAccessWhy+query, "X-Test-Session", userHeader)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: GET = %d", name, rec.Code)
		}
		body := rec.Body.String()
		mustHave(t, body, `<div class="access-why-answer" data-answer="none">`,
			`<h3 id="access-why-answer" class="access-why-question" tabindex="-1" autofocus>No explanation</h3>`,
			`<p class="access-why-summary" role="status">There is no explanation for this pair. Either it does not exist, or you may not see one of its sessions.</p>`)
		mustNotHave(t, body, "secret-plan", "a session of al", "access-why-checks", "does not list", "no grant", "Blocked by")
		bodies[name] = body
	}
	for name, body := range bodies {
		if body != bodies["not found"] {
			t.Errorf("the page for %q differs from the page for an unknown pair", name)
		}
	}
	// A failure of the Hub is not dressed up as "no explanation".
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, accessFixture()).on(ops.OpAccessExplain, func(map[string]any) (any, error) {
		return nil, &OpError{Status: http.StatusInternalServerError, Code: "internal", Message: "boom"}
	}))
	if body := get(t, adminServer(t), pathAccessWhy+query, "X-Test-Session", userHeader).Body.String(); strings.Contains(body, "No explanation") {
		t.Error("a Hub failure is shown as the neutral answer")
	}
}

var whyOption = regexp.MustCompile(`<option value="([^"]*)"( selected)?>([^<]*)</option>`)

// whySelect returns the option values (unescaped) of one select, in order,
// and the selected one.
func whySelect(t *testing.T, body, name string) (values []string, selected string) {
	t.Helper()
	_, rest, ok := strings.Cut(body, `<select name="`+name+`" required>`)
	if !ok {
		t.Fatalf("no select %q", name)
	}
	rest, _, _ = strings.Cut(rest, "</select>")
	for _, m := range whyOption.FindAllStringSubmatch(rest, -1) {
		v := html.UnescapeString(m[1])
		if v == "" {
			continue
		}
		if html.UnescapeString(m[3]) != v[strings.IndexByte(v, '|')+1:] {
			t.Errorf("option %q is labelled %q", v, html.UnescapeString(m[3]))
		}
		values = append(values, v)
		if m[2] != "" {
			selected = v
		}
	}
	return values, selected
}

// The finding "Why cannot select the sessions most likely to need diagnosis":
// with an empty matrix the selects still list the live sessions the caller
// may see, and a user gets only their own snapshot.
func TestAccessWhySelectsListTheLiveSessionsTheCallerSees(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, ops.AccessMatrixResult{}))
	second := SessionRow{WorkerID: "w1", Worker: "laptop", Owner: "alice", Name: "s10"}
	third := SessionRow{WorkerID: "w1", Worker: "laptop", Owner: "alice", Name: "s9"}
	twin := WorkerRow{ID: "w3", Name: "buildbox", Owner: "carol"}
	twinSession := SessionRow{WorkerID: "w3", Worker: "buildbox", Owner: "carol", Name: "ci"}
	mine := Dashboard{Workers: []WorkerRow{wInfra}, Sessions: []SessionRow{sShell}}
	all := Dashboard{Workers: []WorkerRow{wPrivate, wInfra, twin}, Sessions: []SessionRow{sAgent, sShell, second, third, twinSession}}
	s, src := listServer(t, mine, all)

	body := get(t, s, pathAccessWhy, "X-Test-Session", userHeader).Body.String()
	for _, name := range []string{"source", "target"} {
		if got, _ := whySelect(t, body, name); strings.Join(got, " ") != "w2|ci" {
			t.Errorf("a user's %s select = %q", name, got)
		}
	}
	mustHave(t, body, `<optgroup label="buildbox"><option value="w2|ci">ci</option></optgroup>`)
	mustNotHave(t, body, "fix-tests", "laptop", "There is no session to pick yet")

	body = get(t, s, pathAccessWhy, "X-Test-Session", adminHeader).Body.String()
	for _, name := range []string{"source", "target"} {
		// Workers by name, two of one name told apart by owner; sessions in natural order.
		if got, _ := whySelect(t, body, name); strings.Join(got, " ") != "w2|ci w3|ci w1|fix-tests w1|s9 w1|s10" {
			t.Errorf("an administrator's %s select = %q", name, got)
		}
	}
	mustHave(t, body, `<optgroup label="buildbox (bob)">`, `<optgroup label="buildbox (carol)">`, `<optgroup label="laptop">`)
	if len(src.askedAll) != 2 || src.askedAll[0] || !src.askedAll[1] {
		t.Errorf("snapshots asked with all = %v, want [false true]", src.askedAll)
	}

	// Live sessions and matrix ends are one list without duplicates.
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, ops.AccessMatrixResult{
		Sources: []ops.AccessSource{{WorkerID: "w2", WorkerName: "buildbox", Owner: "bob", Session: "ci"}, {WorkerID: "w2", WorkerName: "buildbox", Owner: "bob", Session: "gone"},
			{WorkerID: "w9", WorkerName: "far", Owner: "eve", Session: "a session of eve", Masked: true}},
		Targets: []ops.AccessTarget{{WorkerID: "w9", WorkerName: "far", Owner: "eve", Session: "a session of eve", Masked: true}},
	}))
	body = get(t, s, pathAccessWhy, "X-Test-Session", userHeader).Body.String()
	if got, _ := whySelect(t, body, "source"); strings.Join(got, " ") != "w2|ci w2|gone" {
		t.Errorf("merged select = %q", got)
	}
	mustNotHave(t, body, "w9|", "a session of eve")

	// Nothing at all: the page says so instead of an empty form.
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, ops.AccessMatrixResult{}))
	SetLiveSource(nil)
	body = get(t, adminServer(t), pathAccessWhy, "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, "There is no session to pick yet")
	mustNotHave(t, body, "<select")
}

// Names with quotes, ampersands, angle brackets and the "|" delimiter go from
// the select through the query string to the operation and back unchanged.
func TestAccessWhyNamesRoundTripThroughTheSelectsAndTheQuery(t *testing.T) {
	names := []string{`a"b`, `it's`, `r&d`, `a&amp;b`, `<script>alert(1)</script>`, `left|right`, `|lead`, `tail|`, `a b+c%7Cd`, `x=1&target=wB|y#z`, "caf\u00e9 \u2603"}
	d := Dashboard{Workers: []WorkerRow{{ID: "wA", Name: `box "one" <&>`, Owner: "root"}, {ID: "wB", Name: "box|two", Owner: "root"}}}
	for _, n := range names {
		d.Sessions = append(d.Sessions, SessionRow{WorkerID: "wA", Name: n}, SessionRow{WorkerID: "wB", Name: n})
	}
	f := newFakeOps().reply(ops.OpAccessMatrix, ops.AccessMatrixResult{}).on(ops.OpAccessExplain, func(a map[string]any) (any, error) {
		str := func(k string) string { v, _ := a[k].(string); return v }
		return ops.AccessExplainResult{
			Source: ops.AccessSource{WorkerID: str("source_worker_id"), WorkerName: `box "one" <&>`, Session: str("source_session")},
			Target: ops.AccessTarget{WorkerID: str("target_worker_id"), WorkerName: "box|two", Session: str("target_session")},
			Checks: []ops.AccessCheck{{ID: ops.AccessCheckGrant, State: ops.AccessCheckFail, Text: `no grant for "` + str("target_session") + `"`}},
		}, nil
	})
	connect(t, f)
	s, _ := listServer(t, d, d)
	form := get(t, s, pathAccessWhy, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, form, `<optgroup label="box &#34;one&#34; &lt;&amp;&gt;">`, `<optgroup label="box|two">`)
	mustNotHave(t, form, "<script>alert", `a"b`)
	sources, _ := whySelect(t, form, "source")
	if len(sources) != 2*len(names) {
		t.Fatalf("the select holds %d sessions, want %d: %q", len(sources), 2*len(names), sources)
	}
	for _, n := range names {
		src, tgt := "wA|"+n, "wB|"+n
		if !strings.Contains(strings.Join(sources, "\x00")+"\x00", src+"\x00") {
			t.Errorf("the select does not offer %q", src)
			continue
		}
		// What a browser sends for the chosen options.
		q := url.Values{"source": {src}, "target": {tgt}}.Encode()
		before := len(f.called(ops.OpAccessExplain))
		body := get(t, s, pathAccessWhy+"?"+q, "X-Test-Session", adminHeader).Body.String()
		calls := f.called(ops.OpAccessExplain)
		if len(calls) != before+1 {
			t.Fatalf("%q: no explain call", n)
		}
		a := calls[len(calls)-1].Args
		if a["source_worker_id"] != "wA" || a["source_session"] != n || a["target_worker_id"] != "wB" || a["target_session"] != n {
			t.Errorf("%q reached the operation as %v", n, a)
		}
		if _, sel := whySelect(t, body, "source"); sel != src {
			t.Errorf("%q: the source select shows %q as chosen", n, sel)
		}
		if _, sel := whySelect(t, body, "target"); sel != tgt {
			t.Errorf("%q: the target select shows %q as chosen", n, sel)
		}
		_, question, _ := strings.Cut(body, `tabindex="-1" autofocus>`)
		question, _, _ = strings.Cut(question, "</h3>")
		if got, want := html.UnescapeString(question), "Why can "+n+` on box "one" <&> not reach `+n+" on box|two?"; got != want {
			t.Errorf("the answer is headed %q, want %q", got, want)
		}
		mustHave(t, body, `data-answer="blocked"`)
		mustNotHave(t, body, "<script>alert", "No explanation")
	}
	// A value without the delimiter, or with an empty half, asks nothing.
	before := len(f.called(ops.OpAccessExplain))
	for _, q := range []string{"source=wA&target=wB%7Cx", "source=%7Cx&target=wB%7Cx", "source=wA%7C&target=wB%7Cx", "source=wA%7Cx"} {
		body := get(t, s, pathAccessWhy+"?"+q, "X-Test-Session", adminHeader).Body.String()
		mustNotHave(t, body, "access-why-answer")
	}
	if len(f.called(ops.OpAccessExplain)) != before {
		t.Error("an incomplete pair was sent to the operation")
	}
}

// The optional members of a check: "code" becomes a data attribute, "fix" a
// link, and only a path of this Hub is a link.
func TestAccessWhyRendersTheReasonCodeAndTheFixLinkWhenTheOperationGivesThem(t *testing.T) {
	check := func(id, state string, extra map[string]any) map[string]any {
		m := map[string]any{"id": id, "state": state, "text": id + " text"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	answer := map[string]any{
		"source": map[string]any{"worker_id": "wA", "worker_name": "boss-box", "session": "boss"},
		"target": map[string]any{"worker_id": "wB", "worker_name": "ci", "session": "<i>x</i>"},
		"checks": []any{
			check("caller", "pass", map[string]any{"code": "caller_claimed", "fix": "/workers/wA"}),
			check("grant", "pass", map[string]any{"fix": map[string]any{"href": "/access/grants?q=a&b=<c>", "label": `Open "Grants" <now>`}}),
			check("authority", "pass", map[string]any{"fix": map[string]any{"url": "/admin/users", "text": "Open People"}}),
			check("time", "pass", map[string]any{"code": `x" onmouseover="y`, "fix": "javascript:alert(1)"}),
			check("sharing", "fail", map[string]any{"code": "not_shared", "fix": map[string]any{"href": "/workers/wB/share", "label": "Open Sharing on ci"}}),
			check("workers", "skip", map[string]any{"fix": "//evil.example/x"}),
			check("preview_only", "unknown", map[string]any{"fix": "https://evil.example/x"}),
			check("future <layer>", "odd state", map[string]any{"fix": 7}),
		},
	}
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, accessFixture()).reply(ops.OpAccessExplain, answer))
	body := get(t, adminServer(t), pathAccessWhy+whyPairQuery, "X-Test-Session", userHeader).Body.String()
	mustHave(t, body,
		`data-check="caller" data-state="pass" data-code="caller_claimed">`, `<a class="access-why-fix" href="/workers/wA">Fix this</a>`,
		`<a class="access-why-fix" href="/access/grants?q=a&amp;b=%3cc%3e">Open &#34;Grants&#34; &lt;now&gt;</a>`,
		`<a class="access-why-fix" href="/admin/users">Open People</a>`,
		`data-code="x&#34; onmouseover=&#34;y"`,
		`<li class="access-why-fail access-why-blocking" data-check="sharing" data-state="fail" data-code="not_shared">`,
		`<span class="access-why-first">Blocked by this check</span> <a class="access-why-fix" href="/workers/wB/share">Open Sharing on ci</a></li>`,
		`<span class="badge" data-tone="danger">Blocked by</span> <span>Sharing: sharing text</span>`,
		// an unknown layer and state still render, as "unknown"
		`<li class="access-why-unknown" data-check="future &lt;layer&gt;" data-state="unknown"><span class="access-why-mark">? unknown</span> <span class="access-why-id">future &lt;layer&gt;</span>`)
	mustNotHave(t, body, "javascript:", "evil.example", `onmouseover="y"`)
	if n := strings.Count(body, `class="access-why-fix"`); n != 4 {
		t.Errorf("%d fix links, want 4", n)
	}
	for raw, want := range map[string]string{`"/a"`: "/a", `"a"`: "", `"/\\evil"`: "", `{"href":"//x"}`: "", `null`: "", `""`: "", `[1]`: ""} {
		if got, _ := accessWhyFix([]byte(raw)); got != want {
			t.Errorf("accessWhyFix(%s) = %q, want %q", raw, got, want)
		}
	}
}
