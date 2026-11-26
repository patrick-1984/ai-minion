package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// A grant row shows only what the matrix credits to that grant: a cell of
// the same endpoints held by another grant says nothing about it, and a
// suspended or expired grant is blocked whatever the matrix says.
func TestGrantEffectCountsOnlyTheCellsOfThatGrant(t *testing.T) {
	m := &ops.AccessMatrixResult{
		Sources: []ops.AccessSource{{WorkerID: "wA", Session: "s"}},
		Targets: []ops.AccessTarget{{WorkerID: "wB", Session: "t1"}, {WorkerID: "wB", Session: "t2"}},
		Cells: []ops.AccessCell{
			{Source: 0, Target: 0, Granted: "control", InEffect: "control", Via: "g-other"},
			{Source: 0, Target: 1, Granted: "control", InEffect: "control", Via: ops.AccessViaMaster},
		},
	}
	wildcard := GrantRow{Grant: ops.Grant{ID: "g-view", SourceWorkerID: "wA", SourceSession: "s", TargetWorkerID: "wB",
		Level: ops.GrantLevelView, State: ops.GrantStateActive}}
	if e := grantEffect(wildcard, m); e.Known || e.Level != "" {
		t.Errorf("a wildcard View grant took the cells of another grant: %+v", e)
	}
	for _, state := range []string{ops.GrantStateSuspended, ops.GrantStateExpired} {
		g := wildcard
		g.ID, g.State = "g-other", state
		e := grantEffect(g, m)
		if !e.Known || e.Level != "" || e.BlockedBy != state {
			t.Errorf("%s grant with an in-effect cell = %+v, want blocked by %s", state, e, state)
		}
	}
	own := wildcard
	own.ID = "g-other"
	if e := grantEffect(own, m); !e.Known || e.Level != "control" || e.Covered != 1 {
		t.Errorf("the grant's own cell = %+v", e)
	}
}

// A master cell names the level in effect, not always Control.
func TestAccessMatrixMasterCellNamesTheLevelInEffect(t *testing.T) {
	m := accessFixture()
	m.Cells[0].InEffect = ops.AccessLevelMessage
	m.Cells[1].InEffect = ops.AccessLevelView
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, m))
	body := get(t, adminServer(t), pathAccess, "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `title="Message of b1 on ci, via master"`, `M★<span class="sr-only"> message via master</span>`,
		`title="View of b2 on ci, via master"`, `V★<span class="sr-only"> view via master</span>`)
	if n := strings.Count(body, "C★"); n != 1 {
		t.Errorf("%d C★ glyphs, want 1 (only the Control cell)", n)
	}
}

// The session panel shows the granted level and the level in effect apart
// when they differ.
func TestSessionAccessPaneShowsGrantedAndEffectiveApart(t *testing.T) {
	s, f := sessionOps(t)
	m := sessionAccessMatrix()
	m.Cells[0].InEffect = ops.AccessLevelMessage // a master that holds Control, Message in effect
	m.Cells[2] = ops.AccessCell{Source: 0, Target: 2, Granted: ops.AccessLevelView, InEffect: ops.AccessLevelView, Via: "g1"}
	f.reply(ops.OpGrantList, sessionAccessGrants()).reply(ops.OpAccessMatrix, m)
	pane := accessPaneOf(t, sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String())
	mustHave(t, pane, `<span class="badge plain">Granted: Control</span> <span class="badge" data-tone="positive">In effect: Message</span>`,
		// the same level once
		`<span class="badge plain">View</span> <span class="badge" data-tone="positive">In effect</span>`)

	state, _, eff := accessGrantState(ops.Grant{Level: ops.GrantLevelControl, State: ops.GrantStateActive},
		&ops.AccessCell{Granted: ops.AccessLevelControl, InEffect: ops.AccessLevelMessage, Via: "g"})
	if state != "In effect: Message" || eff != "Message" {
		t.Errorf("a Control grant with Message in effect = %q / %q", state, eff)
	}
}

// A tick whose acceptance lookup fails writes nothing and answers the row
// with the reason, and the page still refreshes; a failed readback goes to
// the status region and reloads the page.
func TestMinionControlFailedLookupKeepsTheRow(t *testing.T) {
	lists := 0
	f := newFakeOps().reply(ops.OpControlSetAccept, ops.WriteResult{Outcome: ops.OutcomeDelivered}).on(ops.OpControlList, func(map[string]any) (any, error) {
		if lists++; lists == 1 {
			return nil, &OpError{Status: 503, Code: "unavailable", Message: "the Hub is busy"}
		}
		return controlFixture(), nil
	})
	connect(t, f)
	s := adminServer(t)
	tick := url.Values{"worker_id": {"wB"}, "source_worker_id": {"wT"}, "value": {"on"}}
	rec := post(t, s, pathMinionControl+"/accept", adminHeader, tick)
	if len(f.called(ops.OpControlSetAccept)) != 0 {
		t.Fatal("a tick without the stored acceptance was written")
	}
	if rec.Header().Get("HX-Trigger") != "control-changed" || rec.Header().Get("HX-Retarget") != "" {
		t.Errorf("headers = %v", rec.Header())
	}
	mustHave(t, rec.Body.String(), `id="ctl-wB"`, "the Hub could not say what this Worker accepts now")
	mustNotHave(t, rec.Body.String(), "Saved:")

	// Every read fails: the row stays, the status region says so, the page reloads.
	f = newFakeOps().on(ops.OpControlList, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 503, Code: "unavailable", Message: "the Hub is busy"}
	})
	connect(t, f)
	rec = post(t, s, pathMinionControl+"/accept", adminHeader, tick)
	if rec.Code != 200 || rec.Header().Get("HX-Retarget") != "#control-status" || rec.Header().Get("HX-Reswap") != "innerHTML" ||
		rec.Header().Get("HX-Trigger") != controlReloadEvent {
		t.Fatalf("answer = %d %v", rec.Code, rec.Header())
	}
	mustHave(t, rec.Body.String(), "Nothing was changed, and the Workers could not be read: the Hub is busy. Reloading what the Hub has saved.")
	mustNotHave(t, rec.Body.String(), "<p", "<tr")
	f = newFakeOps().reply(ops.OpControlSetWorker, ops.WriteResult{Outcome: ops.OutcomeDelivered}).on(ops.OpControlList, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 503, Code: "unavailable", Message: "the Hub is busy"}
	})
	connect(t, f)
	rec = post(t, s, pathMinionControl+"/user-managed", adminHeader, url.Values{"worker_id": {"wB"}})
	mustHave(t, rec.Body.String(), "The change was sent, but the Workers could not be read after it")
	connect(t, newFakeOps().reply(ops.OpControlList, controlFixture()))
	page := get(t, s, pathMinionControl, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, page, `<p id="control-status" class="muted" role="status" aria-live="polite"></p>`)
	js := get(t, s, "/static/minion-control.js").Body.String()
	mustHave(t, js, `addEventListener("control-reload"`, "window.location.reload()")
}

// One rule decides what a row offers and what the handler accepts: the
// level of an administrator grant is for administrators; either owner may
// revoke.
func TestGrantEditabilityIsOneRuleForRowAndHandler(t *testing.T) {
	f := newFakeOps().reply(ops.OpGrantList, grantFixture()).reply(ops.OpGrantSet, ops.GrantSetResult{}).reply(ops.OpGrantRevoke, ops.WriteResult{})
	connect(t, f)
	s := adminServer(t)
	body := get(t, s, pathAccessGrants, "X-Test-Session", ownerHeader).Body.String()
	g1, g2, _ := strings.Cut(body, `<tr id="grant-g2"`)
	// g1: owner authority, alice owns both Workers.
	mustHave(t, g1, `id="grant-g1-level"`, `id="grant-g1-revoke"`)
	// g2: an administrator's wildcard grant from alice's Worker to anna's.
	mustHave(t, g2, `id="grant-g2-revoke"`)
	mustNotHave(t, g2, `id="grant-g2-level"`, `id="grant-g2-save"`, `id="grant-g2-approve"`)
	rec := post(t, s, pathAccessGrants+"/set-level", ownerHeader, url.Values{"id": {"g2"}, "if_revision": {"7"}, "level": {"view"}, "confirm": {"1"}})
	mustHave(t, rec.Body.String(), `<tr id="grant-g2"`, "only an administrator")
	if len(f.called(ops.OpGrantSet)) != 0 {
		t.Error("the handler sent an edit the row does not offer")
	}
	// bob owns neither Worker of g1: nothing is offered and nothing is sent.
	rec = post(t, s, pathAccessGrants+"/revoke", userHeader, url.Values{"id": {"g1"}, "if_revision": {"3"}})
	mustHave(t, rec.Body.String(), `<tr id="grant-g1"`, "only an administrator or the owner of one of its Workers")
	if len(f.called(ops.OpGrantRevoke)) != 0 {
		t.Error("a revoke by a stranger was sent")
	}
	post(t, s, pathAccessGrants+"/revoke", ownerHeader, url.Values{"id": {"g2"}, "if_revision": {"7"}})
	if len(f.called(ops.OpGrantRevoke)) != 1 {
		t.Error("the source owner could not revoke")
	}
	// An administrator may edit every grant.
	admin := get(t, s, pathAccessGrants, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, admin, `id="grant-g1-level"`, `id="grant-g2-level"`)
}

// Minion control is an administrator page: ordinary users get the words, not
// the link.
func TestMinionControlLinksOnlyForAdministrators(t *testing.T) {
	empty := ops.AccessMatrixResult{Sources: []ops.AccessSource{}, Targets: []ops.AccessTarget{}, Cells: []ops.AccessCell{}}
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, empty))
	s := adminServer(t)
	user := get(t, s, pathAccess, "X-Test-Session", userHeader).Body.String()
	mustHave(t, user, "No session reaches another Worker yet. Master sessions and the Workers that accept them are set in Minion control, by an administrator.")
	mustNotHave(t, user, `href="/access/control"`)
	admin := get(t, s, pathAccess, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, admin, `<a href="/access/control">Minion control</a>`)

	answer := map[string]any{
		"source": map[string]any{"worker_id": "wA", "worker_name": "boss-box", "session": "boss"},
		"target": map[string]any{"worker_id": "wB", "worker_name": "ci", "session": "<i>x</i>"},
		"checks": []any{
			map[string]any{"id": "caller", "state": "pass", "text": "caller text"},
			map[string]any{"id": "grant", "state": "fail", "text": "No master session reaches it.", "fix": map[string]any{"href": "/access/control?worker=wB", "label": "Open Minion control"}},
		},
	}
	connect(t, newFakeOps().reply(ops.OpAccessMatrix, accessFixture()).reply(ops.OpAccessExplain, answer))
	user = get(t, s, pathAccessWhy+whyPairQuery, "X-Test-Session", userHeader).Body.String()
	mustHave(t, user, "No master session reaches it.")
	mustNotHave(t, user, "/access/control", "Open Minion control")
	admin = get(t, s, pathAccessWhy+whyPairQuery, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, admin, `<a class="access-why-fix" href="/access/control?worker=wB">Open Minion control</a>`)
	for href, want := range map[string]bool{"/access/control": true, "/access/control#x": true, "/access/control/accept": true, "/access/controls": false, "/access/grants": false} {
		if minionControlLink(href) != want {
			t.Errorf("minionControlLink(%q) = %v", href, !want)
		}
	}
}
