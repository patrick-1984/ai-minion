package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// sessionAccessGrants are grants around the session minion-1 of pi-1: two it
// holds, one that reaches it through the whole Worker, two that are about
// other sessions of the Worker.
func sessionAccessGrants() ops.GrantListResult {
	return ops.GrantListResult{Grants: []ops.Grant{
		{ID: "g1", SourceWorkerID: sessWorkerID, SourceSession: "minion-1", TargetWorkerID: "wT", TargetSession: "<i>t1</i>",
			TargetWorkerName: "ci", TargetOwner: "al", Level: ops.GrantLevelView, State: ops.GrantStateActive},
		{ID: "g2", SourceWorkerID: sessWorkerID, SourceSession: "minion-1", TargetWorkerID: "wT",
			TargetWorkerName: "ci", TargetOwner: "al", Level: ops.GrantLevelMessage, State: ops.GrantStateSuspended, Suspended: true},
		{ID: "g3", SourceWorkerID: "wS", SourceSession: "lead", SourceWorkerName: "boss-box", SourceOwner: "<b>root</b>",
			TargetWorkerID: sessWorkerID, Level: ops.GrantLevelControl, State: ops.GrantStateActive},
		{ID: "g4", SourceWorkerID: "wS", SourceSession: "other-source", SourceWorkerName: "boss-box",
			TargetWorkerID: sessWorkerID, TargetSession: "elsewhere-9", Level: ops.GrantLevelControl, State: ops.GrantStateActive},
		{ID: "g5", SourceWorkerID: sessWorkerID, SourceSession: "sibling-7", TargetWorkerID: "wT", TargetSession: "t2",
			TargetWorkerName: "ci", Level: ops.GrantLevelView, State: ops.GrantStateActive},
	}}
}

// sessionAccessMatrix: minion-1 is a master that reaches b1 on ci, a masked
// master of carol would reach minion-1 but the session is not shared, and the
// grant g1 is blocked by an offline Worker.
func sessionAccessMatrix() ops.AccessMatrixResult {
	return ops.AccessMatrixResult{
		Sources: []ops.AccessSource{
			{WorkerID: sessWorkerID, WorkerName: "pi-1", Owner: "bob", Session: "minion-1", Master: true},
			{WorkerID: "wM", WorkerName: "far", Owner: "carol", Session: "a session of carol", Master: true, Masked: true},
		},
		Targets: []ops.AccessTarget{
			{WorkerID: "wT", WorkerName: "ci", Owner: "al", Online: true, Session: "b1"},
			{WorkerID: sessWorkerID, WorkerName: "pi-1", Owner: "bob", Online: true, Session: "minion-1"},
			{WorkerID: "wT", WorkerName: "ci", Owner: "al", Session: "<i>t1</i>"},
		},
		Cells: []ops.AccessCell{
			{Source: 0, Target: 0, Granted: ops.AccessLevelControl, InEffect: ops.AccessLevelControl, Via: ops.AccessViaMaster},
			{Source: 1, Target: 1, Granted: ops.AccessLevelControl, Via: ops.AccessViaMaster, BlockedBy: ops.AccessBlockedNotShared},
			{Source: 0, Target: 2, Granted: ops.AccessLevelView, Via: "g1", BlockedBy: ops.AccessBlockedWorkerOffline},
		},
	}
}

// accessPaneOf cuts the Access view out of a session page.
func accessPaneOf(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `<div class="access-pane">`)
	if i < 0 {
		t.Fatalf("the session page has no Access view:\n%s", body)
	}
	rest := body[i:]
	if j := strings.Index(rest, `class="access-pane-actions"`); j >= 0 {
		if k := strings.Index(rest[j:], "</p>"); k >= 0 {
			return rest[:j+k]
		}
	}
	return rest
}

func TestSessionAccessPaneListsBothDirections(t *testing.T) {
	s, f := sessionOps(t)
	f.reply(ops.OpGrantList, sessionAccessGrants()).reply(ops.OpAccessMatrix, sessionAccessMatrix())
	rec := sessGet(t, s, "/s/pi-1/minion-1", sessUser)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	body := rec.Body.String()
	mustHave(t, body, `id="pane-tab-access"`, `<link rel="stylesheet" href="/static/access-panels.css">`)
	if strings.Contains(body, " style=") || strings.Contains(body, "<style") {
		t.Error("the session page with the Access view has an inline style")
	}
	pane := accessPaneOf(t, body)
	reach, reached, found := strings.Cut(pane, `id="access-reached-h"`)
	if !found {
		t.Fatalf("no \"Reachable by\" list:\n%s", pane)
	}
	why := `href="/access/why?source=` + sessWorkerID + `%7Cminion-1&amp;target=`
	// Can reach: the two grants of this session and its master reach.
	mustHave(t, reach, ">Can reach</h3>",
		`<strong>&lt;i&gt;t1&lt;/i&gt;</strong> @ ci <span class="muted">al</span>`, `<span class="badge plain">View</span>`,
		`<span class="badge" data-tone="warning">Blocked: a Worker is offline</span>`, why+`wT%7C%3Ci%3Et1%3C%2Fi%3E"`,
		`all shared sessions @ ci`, `<span class="badge plain">Message</span>`, `<span class="badge" data-tone="danger">Suspended</span>`,
		`href="/access/grants?worker=`+sessWorkerID+`#grant-g2"`,
		`<strong>b1</strong> @ ci`, `<span class="badge plain">Control</span>`, `<span class="badge" data-tone="positive">In effect</span>`,
		`via master`, `via grant`, why+`wT%7Cb1"`)
	mustNotHave(t, reach, "lead", "boss-box", "a session of carol", "sibling-7", "elsewhere-9", "<i>t1</i>")
	if n := strings.Count(reach, ">Why?</a>"); n != 2 {
		t.Errorf("%d Why links in \"Can reach\", want 2 (a grant for every shared session is not one pair)", n)
	}
	// Reachable by: the grant for the whole Worker and the masked master.
	mustHave(t, reached, ">Reachable by</h3>",
		`<strong>lead</strong> @ boss-box <span class="muted">&lt;b&gt;root&lt;/b&gt;</span>`, `<span class="badge" data-tone="positive">Active</span>`,
		`href="/access/why?source=wS%7Clead&amp;target=`+sessWorkerID+`%7Cminion-1"`, `#grant-g3"`,
		`<span class="muted">a session of carol</span> @ far`, `Blocked: the session is not shared`)
	mustNotHave(t, reached, "<b>root</b>", "other-source", "elsewhere-9", "sibling-7", `<strong>b1</strong>`)
	if n := strings.Count(reached, ">Why?</a>"); n != 1 {
		t.Errorf("%d Why links in \"Reachable by\", want 1 (a masked session is never a link)", n)
	}
	mustHave(t, body, `<a class="btn btn-secondary" href="/access/grants?source=`+sessWorkerID+`%7Cminion-1#grant-add">Add grant</a>`)
	for _, op := range []string{ops.OpGrantList, ops.OpAccessMatrix} {
		got := f.called(op)
		if len(got) != 1 || got[0].Args["worker_id"] != sessWorkerID || got[0].User != "bob" {
			t.Errorf("%s calls = %+v (one call, as the signed-in user, limited to the Worker)", op, got)
		}
	}
	for _, op := range []string{ops.OpGrantSet, ops.OpGrantRevoke, ops.OpGrantApprove} {
		if len(f.called(op)) != 0 {
			t.Errorf("the page called %s", op)
		}
	}
}

func TestSessionAccessPaneEmptyStatesAndViewOnly(t *testing.T) {
	s, f := sessionOps(t)
	// A Hub without the two operations has no Access view.
	mustNotHave(t, sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String(), `id="pane-tab-access"`, "access-pane")

	f.reply(ops.OpGrantList, ops.GrantListResult{}).reply(ops.OpAccessMatrix, ops.AccessMatrixResult{})
	body := sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String()
	mustHave(t, accessPaneOf(t, body), `<p class="muted">This session can reach no other session.</p>`,
		`<p class="muted">No other session can reach this session.</p>`, ">Add grant</a>")
	mustNotHave(t, accessPaneOf(t, body), "<ul", "Why?")

	// One operation fails: the other half is shown and the failure is said.
	f.on(ops.OpAccessMatrix, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 500, Code: "internal", Message: "boom <b>"}
	})
	mustHave(t, accessPaneOf(t, sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String()),
		"The reach of master sessions could not be read just now.", "can reach no other session")

	// A person who may only view the session is not offered "Add grant".
	f.reply(ops.OpAccessMatrix, ops.AccessMatrixResult{})
	foreign := sessionWorker()
	foreign.OwnerID, foreign.Owner = "u-carol", "carol"
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{foreign}})
	pane := accessPaneOf(t, sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String())
	mustHave(t, pane, "can reach no other session")
	mustNotHave(t, pane, "Add grant", "grant-add")
}

// workerAccessServer serves the pages over a fake caller for worker w1
// ("laptop", alice's): its control state and the grants that touch it.
func workerAccessServer(t *testing.T, control ops.ControlListResult, grants ops.GrantListResult, infos ...ops.WorkerInfo) (*Server, *fakeOps) {
	t.Helper()
	if len(infos) == 0 {
		infos = []ops.WorkerInfo{wAliceInfo}
	}
	f := newFakeOps().
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: infos}).
		reply(ops.OpControlList, control).
		reply(ops.OpGrantList, grants)
	connect(t, f)
	s, _ := listServer(t, snapMine, snapAll)
	return s, f
}

func workerAccessControl(own ops.ControlWorker) ops.ControlListResult {
	own.WorkerID, own.Name = "w1", "laptop"
	return ops.ControlListResult{Workers: []ops.ControlWorker{own,
		{WorkerID: "wX", Name: "boss<b>"}, {WorkerID: "wY", Name: "ci"}, {WorkerID: "wZ", Name: "lab"}}}
}

var workerAccessGrants = ops.GrantListResult{Grants: []ops.Grant{
	{ID: "g1", SourceWorkerID: "w1", SourceSession: "a", TargetWorkerID: "wX"},
	{ID: "g2", SourceWorkerID: "wY", SourceSession: "b", TargetWorkerID: "w1"},
	{ID: "g3", SourceWorkerID: "w1", SourceSession: "a", TargetWorkerID: "w1", TargetSession: "c"},
}}

func TestWorkerAccessPanelForAnAdministrator(t *testing.T) {
	two := []string{"s1", "s2"}
	none := []string{}
	s, f := workerAccessServer(t, workerAccessControl(ops.ControlWorker{Accepts: []ops.ControlAccept{
		{SourceWorkerID: "wX"}, {SourceWorkerID: "wY", Sessions: &two}, {SourceWorkerID: "wZ", Sessions: &none}}}), workerAccessGrants)
	rec := get(t, s, "/w/w1/access", "X-Test-Session", "alice:admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	body := rec.Body.String()
	mustHave(t, body, "<h3>Accepts master sessions from</h3>", `<li>boss&lt;b&gt;</li>`,
		`<li>ci <span class="badge" data-tone="warning">limited (2)</span><span class="sr-only"> laptop accepts only 2 sessions from ci</span></li>`,
		"Except:", `<li>lab <span class="badge" data-tone="danger">blocked</span><span class="sr-only"> laptop accepts nothing from lab</span></li>`,
		`<a href="/access/control" aria-label="Change what laptop accepts">Change</a>`,
		"3 grants touch this Worker", "(2 from its sessions, 2 to its sessions)", `<a href="/access/grants?worker=w1">Show the grants</a>`)
	mustNotHave(t, body, "boss<b>", "All Workers", "Nobody", "<form", "<input", "<button", " style=", "<style", "<script")
	if got := f.called(ops.OpGrantList); len(got) != 1 || got[0].Args["worker_id"] != "w1" || got[0].User != "alice" {
		t.Errorf("grant_list calls = %+v", got)
	}
	if len(f.called(ops.OpControlList)) != 1 {
		t.Errorf("control_list calls = %+v", f.called(ops.OpControlList))
	}

	// All Workers, with the limited source as the exception.
	f.reply(ops.OpControlList, workerAccessControl(ops.ControlWorker{AcceptAll: true, Accepts: []ops.ControlAccept{
		{SourceWorkerID: "wX"}, {SourceWorkerID: "wY", Sessions: &two}}}))
	body = get(t, s, "/w/w1/access", "X-Test-Session", "alice:admin").Body.String()
	mustHave(t, body, "<p>All Workers</p>", "Except:", "limited (2)")
	mustNotHave(t, body, "boss", "Nobody")

	// Nobody, and no grant.
	f.reply(ops.OpControlList, workerAccessControl(ops.ControlWorker{})).reply(ops.OpGrantList, ops.GrantListResult{})
	body = get(t, s, "/w/w1/access", "X-Test-Session", "alice:admin").Body.String()
	mustHave(t, body, "<p>Nobody</p>", `<p class="muted">No grant touches this Worker.</p>`, `href="/access/grants?worker=w1"`)
	mustNotHave(t, body, "Except:", "All Workers", "from its sessions")
}

func TestWorkerAccessPanelForItsOwnerAndForOthers(t *testing.T) {
	s, f := workerAccessServer(t, workerAccessControl(ops.ControlWorker{AcceptAll: true}), workerAccessGrants)
	rec := get(t, s, "/w/w1/access", "X-Test-Session", "alice:user")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	body := rec.Body.String()
	mustHave(t, body, "<h3>Grants</h3>", "3 grants touch this Worker", `href="/access/grants?worker=w1"`)
	mustNotHave(t, body, "Accepts master sessions from", "All Workers", "/access/control", "Change")
	if len(f.called(ops.OpControlList)) != 0 {
		t.Error("control_list (an administrator operation) was called for the owner")
	}

	// The Worker page loads the panel once, after Sharing, and links the stylesheet.
	page := get(t, s, "/workers/w1", "X-Test-Session", "alice:user").Body.String()
	mustHave(t, page, `<link rel="stylesheet" href="/static/access-panels.css">`, `<h2 id="access-h">Access</h2>`,
		`<div id="access-panel" hx-get="/w/w1/access" hx-trigger="load" hx-swap="innerHTML" aria-live="polite">`)
	if strings.Index(page, `id="share-section"`) > strings.Index(page, `id="access-section"`) {
		t.Error("the Access panel is not after Sharing")
	}
	if strings.Contains(page, " style=") || strings.Contains(page, "<style") {
		t.Error("the Worker page has an inline style")
	}

	// Another owner's Worker: no panel on the page, a plain 404 on the route, no call for it.
	s, f = workerAccessServer(t, ops.ControlListResult{}, workerAccessGrants, wBobInfo)
	if rec := get(t, s, "/w/w2/access", "X-Test-Session", "alice:user"); rec.Code != http.StatusNotFound {
		t.Errorf("GET of another owner's Worker = %d, want 404", rec.Code)
	}
	if len(f.called(ops.OpGrantList))+len(f.called(ops.OpControlList)) != 0 {
		t.Error("an operation was called for a person who may not operate the Worker")
	}
}

func TestAccessPanelsStylesheetUsesTokensOnly(t *testing.T) {
	rec := get(t, adminServer(t), "/static/access-panels.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	css := rec.Body.String()
	mustHave(t, css, ".access-rows", ".access-worker", "var(--border)")
	mustNotHave(t, css, "#", "rgb(", "hsl(", "!important")
}
