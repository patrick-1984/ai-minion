package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func grantFixture() ops.GrantListResult {
	return ops.GrantListResult{Grants: []ops.Grant{
		{ID: "g1", SourceWorkerID: "wA", SourceSession: "wb-4", SourceProof: ops.GrantProofClaimed, TargetWorkerID: "wB",
			TargetSession: "wb-3", Level: ops.GrantLevelView, Authority: ops.GrantAuthorityOwner, GrantedBy: "u-alice", Revision: 3,
			SourceWorkerName: "work<b>", TargetWorkerName: "coder", SourceOwner: "alice", TargetOwner: "alice", State: ops.GrantStateActive},
		{ID: "g2", SourceWorkerID: "wA", SourceSession: "ci", SourceProof: ops.GrantProofVerified, TargetWorkerID: "wC",
			Level: ops.GrantLevelControl, Authority: ops.GrantAuthorityAdmin, GrantedBy: "u-root", Revision: 7, Suspended: true,
			SourceWorkerName: "work<b>", TargetWorkerName: "anna-laptop", SourceOwner: "alice", TargetOwner: "anna", State: ops.GrantStateSuspended},
	}}
}

func TestAccessGrantsPageListsGrantsForEveryUser(t *testing.T) {
	f := newFakeOps().reply(ops.OpGrantList, grantFixture()).
		reply(ops.OpUsersList, ops.UsersListResult{Users: []ops.UserInfo{{ID: "u-root", Username: "root"}}})
	connect(t, f)
	s := adminServer(t)
	admin := get(t, s, pathAccessGrants, "X-Test-Session", adminHeader)
	if admin.Code != http.StatusOK {
		t.Fatalf("admin GET = %d", admin.Code)
	}
	body := admin.Body.String()
	mustHave(t, body, `<div class="page-head access-wide">`, `<h1>Access</h1>`, `class="rtable grant-table"`,
		`<tr id="grant-g1" data-filter=`, `<strong>wb-4</strong> @ work&lt;b&gt;`, `<strong>wb-3</strong> @ coder`,
		"Claimed: this session is named by its Worker. Any program running as that Worker's OS user can use this grant.",
		`>Verified</span>`, "all shared sessions of anna-laptop", `<span class="badge" data-tone="info">Active</span>`,
		`<span class="badge" data-tone="danger">Suspended</span>`, `<th scope="col">State</th><th scope="col">In effect</th>`,
		`alice <span class="muted">owner</span>`, `root <span class="muted">administrator</span>`, `<td data-label="Until">no end</td>`,
		`<tr id="grant-g2" data-filter="ci work&lt;b&gt; alice  anna-laptop anna control root" data-between="1" data-off="1">`,
		`hx-post="/access/grants/set-level" hx-target="closest tr" hx-swap="outerHTML" hx-sync="#grant-panel:queue all" hx-disabled-elt="find button"`,
		`id="grant-g1-level"`, `id="grant-g1-save"`, `id="grant-g1-revoke"`, `id="grant-g2-approve"`, `<div id="grant-status" class="grant-page-status" aria-live="polite"></div>`,
		`<link rel="stylesheet" href="/static/access-grants.css">`,
		`name="if_revision" value="7"`, `<option value="message">Message</option>`,
		`hx-post="/access/grants/approve"`, `hx-post="/access/grants/revoke"`,
		`<tbody id="grant-rows" hx-get="/access/grants" hx-select="#grant-rows" hx-swap="outerHTML" hx-trigger="grants-changed from:body" hx-sync="#grant-panel:queue last">`,
		`<dialog id="grant-dialog"`, `<details class="panel grant-add" id="grant-add" hidden>`, `id="grant-only-between"`, `id="grant-only-off"`,
		`<script src="/static/access-grants.js" defer></script>`, `href="/access/grants"`)
	mustNotHave(t, body, "work<b>", "hx-trigger=\"change\"")

	// Any signed-in user gets the page; Approve is for administrators.
	user := get(t, s, pathAccessGrants, "X-Test-Session", userHeader)
	if user.Code != http.StatusOK {
		t.Fatalf("user GET = %d", user.Code)
	}
	mustHave(t, user.Body.String(), `<tr id="grant-g2"`, `hx-post="/access/grants/revoke"`)
	mustNotHave(t, user.Body.String(), "/access/grants/approve", ">Approve<")
	if rec := post(t, s, pathAccessGrants+"/approve", userHeader, url.Values{"id": {"g2"}, "if_revision": {"7"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("user approve = %d", rec.Code)
	}
	if len(f.called(ops.OpGrantApprove)) != 0 {
		t.Error("a user's approve reached the Hub")
	}

	css := get(t, s, "/static/minion.css").Body.String()
	mustHave(t, css, "/* access: grants */", "main:has(> .access-wide) { max-width: none; }", "/* end access: grants */")
}

func TestAccessGrantsEmptyStateOffersAdd(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpGrantList, ops.GrantListResult{}))
	body := get(t, adminServer(t), pathAccessGrants, "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `class="empty-state"`, "There is no grant yet", `data-grant-add hidden>Add grant</button>`)
	mustNotHave(t, body, `id="grant-rows"`)
}

func TestAccessGrantRevokeSendsTheRevisionAndAnswersTheRow(t *testing.T) {
	// Still listed (a Hub that keeps the row): the row comes back with a status.
	f := newFakeOps().reply(ops.OpGrantList, grantFixture()).reply(ops.OpGrantRevoke, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	rec := post(t, adminServer(t), pathAccessGrants+"/revoke", userHeader, url.Values{"id": {"g1"}, "if_revision": {"3"}})
	if rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	calls := f.called(ops.OpGrantRevoke)
	if len(calls) != 1 || calls[0].Args["id"] != "g1" || calls[0].Args["if_revision"] != float64(3) || calls[0].Args["request_id"] == "" || calls[0].Args["request_id"] == nil {
		t.Fatalf("calls = %+v", calls)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, `<tr id="grant-g1"`) {
		t.Fatalf("the answer is not the row: %s", body)
	}
	mustHave(t, body, `class="grant-status" role="status">Revoked</p>`)

	// No longer listed: a row that says so takes its place.
	f = newFakeOps().reply(ops.OpGrantList, ops.GrantListResult{}).reply(ops.OpGrantRevoke, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	body = post(t, adminServer(t), pathAccessGrants+"/revoke", userHeader, url.Values{"id": {"g1"}, "if_revision": {"3"}}).Body.String()
	mustHave(t, body, `<tr id="grant-g1" class="grant-gone">`, `role="status">Revoked</span>`)
	mustNotHave(t, body, "<form")
}

func TestAccessGrantSetLevelCallsGrantSetAndShowsAConflictInTheRow(t *testing.T) {
	f := newFakeOps().reply(ops.OpGrantList, grantFixture()).reply(ops.OpGrantSet, ops.GrantSetResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	form := url.Values{"id": {"g1"}, "if_revision": {"3"}, "level": {"control"}, "source_worker_id": {"wA"}, "source_session": {"wb-4"},
		"target_worker_id": {"wB"}, "target_session": {"wb-3"}, "confirm": {"1"}}
	rec := post(t, adminServer(t), pathAccessGrants+"/set-level", userHeader, form)
	calls := f.called(ops.OpGrantSet)
	if len(calls) != 1 || calls[0].Args["id"] != "g1" || calls[0].Args["if_revision"] != float64(3) || calls[0].Args["level"] != "control" ||
		calls[0].Args["source_session"] != "wb-4" || calls[0].Args["target_worker_id"] != "wB" {
		t.Fatalf("calls = %+v", calls)
	}
	// The row is what the Hub lists (still View in the fixture), never the posted value.
	mustHave(t, rec.Body.String(), `<tr id="grant-g1"`, `<option value="view" selected>View</option>`, `role="status">Saved</p>`)
	if rec.Header().Get("HX-Trigger") != "" {
		t.Error("a row action reloads the whole list: its status line would be lost")
	}

	f = newFakeOps().reply(ops.OpGrantList, grantFixture()).on(ops.OpGrantSet, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 409, Code: "conflict", Message: "revision mismatch"}
	})
	connect(t, f)
	body := post(t, adminServer(t), pathAccessGrants+"/set-level", userHeader, form).Body.String()
	mustHave(t, body, `<tr id="grant-g1"`, `class="grant-status grant-error" role="alert">Changed by someone else. This is the current state.</p>`,
		`hx-post="/access/grants/revoke"`)
	mustNotHave(t, body, `role="status">Saved`)

	f = newFakeOps().reply(ops.OpGrantList, grantFixture()).on(ops.OpGrantSet, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 403, Code: "forbidden", Message: "only the owner of the target may grant"}
	})
	connect(t, f)
	body = post(t, adminServer(t), pathAccessGrants+"/set-level", userHeader, form).Body.String()
	mustHave(t, body, `<tr id="grant-g1"`, "only the owner of the target may grant")
}

func TestAccessGrantApproveIsSentForAnAdministrator(t *testing.T) {
	f := newFakeOps().reply(ops.OpGrantList, grantFixture()).reply(ops.OpGrantApprove, ops.GrantSetResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	rec := post(t, adminServer(t), pathAccessGrants+"/approve", adminHeader, url.Values{"id": {"g2"}, "if_revision": {"7"}})
	calls := f.called(ops.OpGrantApprove)
	if len(calls) != 1 || calls[0].Args["id"] != "g2" || calls[0].Args["if_revision"] != float64(7) {
		t.Fatalf("calls = %+v", calls)
	}
	mustHave(t, rec.Body.String(), `<tr id="grant-g2"`, `role="status">Approved</p>`)
}

func TestAccessGrantAddPostsGrantSetWithTheChosenMembers(t *testing.T) {
	f := newFakeOps().reply(ops.OpGrantList, grantFixture()).reply(ops.OpGrantSet, ops.GrantSetResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	d := Dashboard{Workers: []WorkerRow{wPrivate, wInfra}, Sessions: []SessionRow{sAgent, sShell}}
	s, _ := listServer(t, d, d)

	// The dialog lists the live sessions; a user starts only from an own Worker.
	admin := get(t, s, pathAccessGrants, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, admin, `<option value="w1|fix-tests" data-worker="w1">fix-tests @ laptop</option>`,
		`<input type="radio" name="level" id="grant-add-level-message" value="message">`,
		`name="until" value="30"`, `hx-post="/access/grants/add" hx-target="#grant-add-result"`)
	user := get(t, s, pathAccessGrants, "X-Test-Session", userHeader).Body.String()
	_, sources, _ := strings.Cut(user, `name="source_worker_id"`)
	sources, _, _ = strings.Cut(sources, `name="target_worker_id"`)
	mustHave(t, sources, `<option value="w2">buildbox (bob)</option>`)
	mustNotHave(t, sources, `value="w1"`)

	before := time.Now().UTC()
	add := url.Values{"source_worker_id": {"w1"}, "source_session": {"w1|fix-tests"},
		"target_worker_id": {"w2"}, "target_scope": {"all"}, "target_session": {""}, "level": {"control"}, "until": {"7"}, "note": {" for the build "}}
	// Control on every shared session of another owner: the first post only asks.
	rec := post(t, s, pathAccessGrants+"/add", adminHeader, add)
	if len(f.called(ops.OpGrantSet)) != 0 || rec.Header().Get("HX-Trigger") != "" {
		t.Fatalf("an unconfirmed broad grant was sent: %s", rec.Body)
	}
	mustHave(t, rec.Body.String(), "fix-tests on laptop (alice) will get Control: it can see, send prompts to and type into every session that buildbox (bob) shares, now and later, for 7 days.",
		`<input type="hidden" name="confirm" value="1">`, `id="grant-add-confirm-button">Confirm and add</button>`)
	_, key, _ := strings.Cut(rec.Body.String(), `name="confirm_for" value="`)
	key, _, _ = strings.Cut(key, `"`)
	// A confirmation of other values does not count.
	other := url.Values{"confirm": {"1"}, "confirm_for": {key}}
	for k, v := range add {
		other[k] = v
	}
	other.Set("source_session", "w1|other")
	if post(t, s, pathAccessGrants+"/add", adminHeader, other); len(f.called(ops.OpGrantSet)) != 0 {
		t.Fatal("a confirmation was accepted for changed values")
	}
	add.Set("confirm", "1")
	add.Set("confirm_for", key)
	rec = post(t, s, pathAccessGrants+"/add", adminHeader, add)
	calls := f.called(ops.OpGrantSet)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v (%s)", calls, rec.Body)
	}
	a := calls[0].Args
	if a["source_worker_id"] != "w1" || a["source_session"] != "fix-tests" || a["target_worker_id"] != "w2" || a["target_session"] != nil ||
		a["level"] != "control" || a["note"] != "for the build" || a["id"] != nil || a["if_revision"] != nil || a["request_id"] == nil {
		t.Fatalf("args = %+v", a)
	}
	until, err := time.Parse("2006-01-02T15:04:05.000Z", a["expires_at"].(string))
	if err != nil || until.Before(before.Add(7*24*time.Hour-time.Minute)) || until.After(before.Add(7*24*time.Hour+time.Minute)) {
		t.Fatalf("expires_at = %v (%v)", a["expires_at"], err)
	}
	if rec.Header().Get("HX-Trigger") != "grants-changed" {
		t.Error("the list is not told to reload")
	}
	mustHave(t, rec.Body.String(), `role="status">Saved</p>`)

	// A refusal stays in the dialog; a session of another Worker is not sent.
	f = newFakeOps().on(ops.OpGrantSet, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 403, Code: "forbidden", Message: "you do not own that Worker"}
	})
	connect(t, f)
	rec = post(t, s, pathAccessGrants+"/add", userHeader, url.Values{"source_worker_id": {"w2"}, "source_session": {"w2|ci"},
		"target_worker_id": {"w1"}, "target_scope": {"one"}, "target_session": {"w1|fix-tests"}, "level": {"view"}, "confirm": {"1"},
		"confirm_for": {grantConfirmKeyForTest("w2", "ci", "w1", "one", "fix-tests", "view", "")}})
	mustHave(t, rec.Body.String(), `<p class="form-error" role="alert">you do not own that Worker</p>`)
	if rec.Header().Get("HX-Trigger") != "" {
		t.Error("a refused grant reloads the list")
	}
	rec = post(t, s, pathAccessGrants+"/add", userHeader, url.Values{"source_worker_id": {"w2"}, "source_session": {"w1|fix-tests"},
		"target_worker_id": {"w1"}, "target_scope": {"all"}, "level": {"view"}})
	mustHave(t, rec.Body.String(), "The source session is not on the chosen source Worker.")
	if len(f.called(ops.OpGrantSet)) != 1 {
		t.Error("a mismatched session reached the Hub")
	}
}

func grantConfirmKeyForTest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// The target is never chosen for the user: no preselected wildcard on the
// page, and the server refuses an Add without the explicit choice.
func TestAccessGrantAddNeedsAnExplicitTargetChoice(t *testing.T) {
	f := newFakeOps().reply(ops.OpGrantList, grantFixture()).reply(ops.OpGrantSet, ops.GrantSetResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	wDesk := WorkerRow{ID: "w3", Name: "desk", Owner: "alice", Kind: ops.WorkerKindPrivate, Online: true}
	sDesk := SessionRow{WorkerID: "w3", Worker: "desk", Owner: "alice", Name: "notes", Kind: "shell", Online: true}
	d := Dashboard{Workers: []WorkerRow{wPrivate, wInfra, wDesk}, Sessions: []SessionRow{sAgent, sShell, sDesk}}
	s, _ := listServer(t, d, d)
	page := get(t, s, pathAccessGrants, "X-Test-Session", adminHeader).Body.String()
	_, target, _ := strings.Cut(page, `name="target_worker_id"`)
	target, _, _ = strings.Cut(target, `grant-levels`)
	mustHave(t, target, `<input type="radio" name="target_scope" id="grant-add-scope-one" value="one" required> One session`,
		`<input type="radio" name="target_scope" id="grant-add-scope-all" value="all" required> Every shared session of this Worker`,
		`<select name="target_session" id="grant-add-target-session">
      <option value="">Choose a session</option>`)
	mustNotHave(t, target, "checked", `<option value="">every shared session`, " selected")
	// No native post: the session check wants a header a plain form cannot send.
	mustHave(t, page, `<noscript><p class="muted">Adding or changing a grant needs scripts`, `<form id="grant-add-form" class="grant-form" hx-post="/access/grants/add"`)
	mustNotHave(t, page, `method="post"`, `action="/access/grants/add"`)

	base := url.Values{"source_worker_id": {"w1"}, "source_session": {"w1|fix-tests"}, "target_worker_id": {"w3"}, "level": {"view"}}
	rec := post(t, s, pathAccessGrants+"/add", adminHeader, base)
	mustHave(t, rec.Body.String(), "Choose the target: one session, or every shared session of the target Worker.")
	base.Set("target_scope", "one")
	rec = post(t, s, pathAccessGrants+"/add", adminHeader, base)
	mustHave(t, rec.Body.String(), "Choose the target session.")
	base.Set("target_scope", "every")
	post(t, s, pathAccessGrants+"/add", adminHeader, base)
	if n := len(f.called(ops.OpGrantSet)); n != 0 {
		t.Fatalf("an Add without an explicit target reached the Hub %d times", n)
	}

	// View for one session of the same owner needs no second step.
	base.Set("target_scope", "one")
	base.Set("target_session", "w3|notes")
	rec = post(t, s, pathAccessGrants+"/add", adminHeader, base)
	calls := f.called(ops.OpGrantSet)
	if len(calls) != 1 || calls[0].Args["target_session"] != "notes" || rec.Header().Get("HX-Trigger") != "grants-changed" {
		t.Fatalf("calls = %+v (%s)", calls, rec.Body)
	}
	// Message for one session of the same owner: none either. Control, a
	// wildcard and another owner each ask first.
	for _, c := range []struct {
		level, worker, scope, session string
		confirm                       bool
	}{
		{"message", "w3", "one", "w3|notes", false},
		{"control", "w3", "one", "w3|notes", true},
		{"view", "w3", "all", "", true},
		{"view", "w2", "one", "w2|ci", true},
	} {
		before := len(f.called(ops.OpGrantSet))
		rec = post(t, s, pathAccessGrants+"/add", adminHeader, url.Values{"source_worker_id": {"w1"}, "source_session": {"w1|fix-tests"},
			"target_worker_id": {c.worker}, "target_scope": {c.scope}, "target_session": {c.session}, "level": {c.level}})
		asked := strings.Contains(rec.Body.String(), `id="grant-add-confirm"`)
		if sent := len(f.called(ops.OpGrantSet)) > before; asked != c.confirm || sent == c.confirm {
			t.Errorf("%+v: asked %v, sent %v", c, asked, sent)
		}
	}
}

func TestAccessGrantSetLevelAsksBeforeItWidens(t *testing.T) {
	f := newFakeOps().reply(ops.OpGrantList, grantFixture()).reply(ops.OpGrantSet, ops.GrantSetResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	s := adminServer(t)
	rec := post(t, s, pathAccessGrants+"/set-level", userHeader, url.Values{"id": {"g1"}, "if_revision": {"3"}, "level": {"control"}})
	if len(f.called(ops.OpGrantSet)) != 0 {
		t.Fatal("Control was set without the second step")
	}
	mustHave(t, rec.Body.String(), `<tr id="grant-g1"`,
		"wb-4 on work&lt;b&gt; (alice) will get Control: it can see, send prompts to and type into the session wb-3 on coder (alice), with no end.",
		`<input type="hidden" name="level" value="control"><input type="hidden" name="confirm" value="1">`, `id="grant-g1-confirm">Confirm</button>`,
		`hx-post="/access/grants/show"`, `id="grant-g1-cancel">Cancel</button>`)
	mustNotHave(t, rec.Body.String(), `id="grant-g1-save"`)
	// Cancel answers the row and writes nothing.
	rec = post(t, s, pathAccessGrants+"/show", userHeader, url.Values{"id": {"g1"}})
	mustHave(t, rec.Body.String(), `<tr id="grant-g1"`, `id="grant-g1-save"`)
	// Message for one session of the same owner needs no second step; the
	// change is built from the listed grant, not from the form.
	post(t, s, pathAccessGrants+"/set-level", userHeader, url.Values{"id": {"g1"}, "if_revision": {"3"}, "level": {"message"}, "target_session": {"evil"}})
	calls := f.called(ops.OpGrantSet)
	if len(calls) != 1 || calls[0].Args["level"] != "message" || calls[0].Args["target_session"] != "wb-3" || calls[0].Args["source_worker_id"] != "wA" {
		t.Fatalf("calls = %+v", calls)
	}
	// A stale revision is a conflict before anything is sent.
	rec = post(t, s, pathAccessGrants+"/set-level", userHeader, url.Values{"id": {"g1"}, "if_revision": {"2"}, "level": {"view"}})
	mustHave(t, rec.Body.String(), grantConflictText)
	if len(f.called(ops.OpGrantSet)) != 1 {
		t.Error("a stale row was sent")
	}
}

func TestAccessGrantRowActionsNeedTheGrantAndItsRevision(t *testing.T) {
	f := newFakeOps().reply(ops.OpGrantList, grantFixture()).reply(ops.OpGrantSet, ops.GrantSetResult{}).
		reply(ops.OpGrantRevoke, ops.WriteResult{}).reply(ops.OpGrantApprove, ops.GrantSetResult{})
	connect(t, f)
	s := adminServer(t)
	for _, c := range []struct {
		action string
		form   url.Values
	}{
		{"revoke", url.Values{"id": {"g1"}}},
		{"revoke", url.Values{"id": {"g1"}, "if_revision": {"abc"}}},
		{"revoke", url.Values{"id": {"g1"}, "if_revision": {"0"}}},
		{"revoke", url.Values{"if_revision": {"3"}}},
		{"approve", url.Values{"id": {"g2"}, "if_revision": {"-1"}}},
		{"set-level", url.Values{"if_revision": {"3"}, "level": {"view"}, "confirm": {"1"}}},
		{"set-level", url.Values{"id": {"g1"}, "level": {"view"}, "confirm": {"1"}}},
	} {
		rec := post(t, s, pathAccessGrants+"/"+c.action, adminHeader, c.form)
		if rec.Header().Get("HX-Retarget") != "#grant-status" || rec.Header().Get("HX-Reswap") != "innerHTML" || strings.Contains(rec.Body.String(), "<tr") {
			t.Errorf("%s %v: the answer would replace the row: %v %s", c.action, c.form, rec.Header(), rec.Body)
		}
		mustHave(t, rec.Body.String(), "Nothing was changed", "data-grant-reload")
	}
	if n := len(f.called(ops.OpGrantSet)) + len(f.called(ops.OpGrantRevoke)) + len(f.called(ops.OpGrantApprove)); n != 0 {
		t.Fatalf("%d writes without a grant id or revision", n)
	}
}

func TestAccessGrantFailedReadbackKeepsTheRow(t *testing.T) {
	f := newFakeOps().reply(ops.OpGrantRevoke, ops.WriteResult{Outcome: ops.OutcomeDelivered}).on(ops.OpGrantList, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 503, Code: "unavailable", Message: "the Hub is busy"}
	})
	connect(t, f)
	rec := post(t, adminServer(t), pathAccessGrants+"/revoke", userHeader, url.Values{"id": {"g1"}, "if_revision": {"3"}})
	if rec.Code != 200 || rec.Header().Get("HX-Retarget") != "#grant-status" || rec.Header().Get("HX-Trigger") != `{"grant-unconfirmed":{"id":"g1"}}` {
		t.Fatalf("answer = %d %v", rec.Code, rec.Header())
	}
	mustHave(t, rec.Body.String(), "The change was sent, but the list could not be read after it", "Reload the list")
	mustNotHave(t, rec.Body.String(), "<tr")
	js := get(t, adminServer(t), "/static/access-grants.js").Body.String()
	mustHave(t, js, `"grant-unconfirmed"`, "data-unconfirmed", "htmx:afterSwap", "restoreFocus()", "focusin", `t.id === "grant-rows"`)
}

func TestAccessGrantInEffectComesFromTheMatrix(t *testing.T) {
	list := grantFixture()
	list.Grants[1].Suspended, list.Grants[1].State = false, ops.GrantStateActive
	matrix := ops.AccessMatrixResult{
		Sources: []ops.AccessSource{{WorkerID: "wA", Session: "wb-4"}, {WorkerID: "wA", Session: "ci"}},
		Targets: []ops.AccessTarget{{WorkerID: "wB", Session: "wb-3"}, {WorkerID: "wC", Session: "a"}, {WorkerID: "wC", Session: "b"}},
		Cells: []ops.AccessCell{
			{Source: 0, Target: 0, Granted: "view", Via: "g1", BlockedBy: ops.AccessBlockedNotShared},
			{Source: 1, Target: 1, Granted: "control", InEffect: "control", Via: "g2"},
			{Source: 1, Target: 2, Granted: "control", Via: "g2", BlockedBy: ops.AccessBlockedWorkerOffline},
			{Source: 9, Target: 0, Via: "g1", InEffect: "control"},
		},
	}
	f := newFakeOps().reply(ops.OpGrantList, list).reply(ops.OpAccessMatrix, matrix)
	connect(t, f)
	body := get(t, adminServer(t), pathAccessGrants, "X-Test-Session", adminHeader).Body.String()
	g1, g2, _ := strings.Cut(body, `<tr id="grant-g2"`)
	// Active is the stored state; the matrix says that g1 does not work.
	mustHave(t, g1, `data-tone="info">Active</span>`, `<td data-label="In effect"><span class="chip grant-blocked" title="Blocked by: the session is not shared">Blocked: the session is not shared</span></td>`)
	mustHave(t, g2, `<td data-label="In effect">Control on 1 of 2 sessions</td>`)

	// Without the matrix nothing claims an effect; a suspended grant is blocked anyway.
	connect(t, newFakeOps().reply(ops.OpGrantList, grantFixture()))
	body = get(t, adminServer(t), pathAccessGrants, "X-Test-Session", adminHeader).Body.String()
	g1, g2, _ = strings.Cut(body, `<tr id="grant-g2"`)
	mustHave(t, g1, `data-tone="info">Active</span>`, `title="The Hub could not say whether this grant works now.">unknown</span>`)
	mustHave(t, g2, `Blocked: the grant is suspended`)
	mustNotHave(t, body, ">In effect</span>")
}

func TestAccessGrantConflictsKeepTheirOwnWords(t *testing.T) {
	refuse := func(msg string) func(map[string]any) (any, error) {
		return func(map[string]any) (any, error) { return nil, &OpError{Status: 409, Code: "conflict", Message: msg} }
	}
	f := newFakeOps().reply(ops.OpGrantList, grantFixture()).on(ops.OpGrantApprove, refuse("both Workers must be approved")).
		on(ops.OpGrantRevoke, refuse("the grant was changed meanwhile; reload and decide again")).
		on(ops.OpGrantSet, refuse("a grant for this source and target exists already"))
	connect(t, f)
	d := Dashboard{Workers: []WorkerRow{wPrivate, wInfra}, Sessions: []SessionRow{sAgent, sShell}}
	s, _ := listServer(t, d, d)
	body := post(t, s, pathAccessGrants+"/approve", adminHeader, url.Values{"id": {"g2"}, "if_revision": {"7"}}).Body.String()
	mustHave(t, body, `<tr id="grant-g2"`, `role="alert">both Workers must be approved</p>`)
	mustNotHave(t, body, grantConflictText)
	body = post(t, s, pathAccessGrants+"/revoke", adminHeader, url.Values{"id": {"g1"}, "if_revision": {"3"}}).Body.String()
	mustHave(t, body, `<tr id="grant-g1"`, grantConflictText)
	// Add has no revision: a duplicate is named as one.
	body = post(t, s, pathAccessGrants+"/add", adminHeader, url.Values{"source_worker_id": {"w1"}, "source_session": {"w1|fix-tests"},
		"target_worker_id": {"w2"}, "target_scope": {"one"}, "target_session": {"w2|ci"}, "level": {"view"}, "confirm": {"1"},
		"confirm_for": {grantConfirmKeyForTest("w1", "fix-tests", "w2", "one", "ci", "view", "")}}).Body.String()
	mustHave(t, body, "a grant for this source and target exists already")
	mustNotHave(t, body, grantConflictText)
}

func TestAccessGrantMaskedRowOffersOnlyRevoke(t *testing.T) {
	list := grantFixture()
	list.Grants[1].TargetSession, list.Grants[1].TargetMasked = "a session of anna", true
	list.Grants[1].Suspended, list.Grants[1].State = false, ops.GrantStateActive
	f := newFakeOps().reply(ops.OpGrantList, list).reply(ops.OpGrantSet, ops.GrantSetResult{}).reply(ops.OpGrantRevoke, ops.WriteResult{})
	connect(t, f)
	s := adminServer(t)
	body := get(t, s, pathAccessGrants, "X-Test-Session", adminHeader).Body.String()
	_, g2, _ := strings.Cut(body, `<tr id="grant-g2"`)
	mustHave(t, g2, `<em class="grant-masked">a session of anna</em> @ anna-laptop`, `hx-post="/access/grants/revoke"`, `id="grant-g2-revoke"`, "only Revoke")
	mustNotHave(t, g2, "/access/grants/set-level", "/access/grants/approve", `id="grant-g2-level"`)
	rec := post(t, s, pathAccessGrants+"/set-level", adminHeader, url.Values{"id": {"g2"}, "if_revision": {"7"}, "level": {"view"}, "confirm": {"1"}})
	mustHave(t, rec.Body.String(), `<tr id="grant-g2"`, "it can only be revoked")
	if len(f.called(ops.OpGrantSet)) != 0 {
		t.Error("a masked grant was changed")
	}
	post(t, s, pathAccessGrants+"/revoke", adminHeader, url.Values{"id": {"g2"}, "if_revision": {"7"}})
	if len(f.called(ops.OpGrantRevoke)) != 1 {
		t.Error("a masked grant could not be revoked")
	}
}

// D0: a plain user sees the masked text of grant_list for a session of another
// owner, never a name; the decider of another user stays an id (a user cannot
// list users), and neither Approve nor the audit History is offered.
func TestAccessGrantsMaskedRowsForAPlainUser(t *testing.T) {
	list := ops.GrantListResult{Grants: []ops.Grant{
		{ID: "g9", SourceWorkerID: "wP", SourceSession: "a session of patryk", SourceMasked: true, SourceProof: ops.GrantProofClaimed,
			TargetWorkerID: "wM", TargetSession: "notes", Level: ops.GrantLevelMessage, Authority: ops.GrantAuthorityOwner,
			GrantedBy: "u-patryk", Revision: 2, SourceWorkerName: "patryk-work", TargetWorkerName: "marta-x1",
			SourceOwner: "patryk", TargetOwner: "bob", State: ops.GrantStateActive},
		{ID: "g8", SourceWorkerID: "wM", SourceSession: "notes", SourceProof: ops.GrantProofClaimed,
			TargetWorkerID: "wP", TargetSession: "a session of patryk", TargetMasked: true, Level: ops.GrantLevelView,
			Authority: ops.GrantAuthorityOwner, GrantedBy: "u-bob", Revision: 1, SourceWorkerName: "marta-x1",
			TargetWorkerName: "patryk-work", SourceOwner: "bob", TargetOwner: "patryk", State: ops.GrantStateActive},
	}}
	f := newFakeOps().reply(ops.OpGrantList, list).reply(ops.OpUsersList, ops.UsersListResult{Users: []ops.UserInfo{{ID: "u-patryk", Username: "patryk"}}})
	connect(t, f)
	body := get(t, adminServer(t), pathAccessGrants, "X-Test-Session", userHeader).Body.String()
	_, g9, _ := strings.Cut(body, `<tr id="grant-g9"`)
	g9, _, _ = strings.Cut(g9, "</tr>")
	mustHave(t, g9, `<em class="grant-masked">a session of patryk</em> @ patryk-work`, `<strong>notes</strong> @ marta-x1`,
		`<span class="muted grant-user-id" title="User id: the name is not known to you">u-patryk</span> <span class="muted">owner</span>`,
		"only Revoke", `id="grant-g9-revoke"`)
	mustNotHave(t, g9, `id="grant-g9-level"`, "/access/grants/approve", "History", "/admin/audit")
	_, g8, _ := strings.Cut(body, `<tr id="grant-g8"`)
	g8, _, _ = strings.Cut(g8, "</tr>")
	mustHave(t, g8, `<strong>notes</strong> @ marta-x1`, `<em class="grant-masked">a session of patryk</em> @ patryk-work`,
		`<td data-label="Decided by">bob <span class="muted">owner</span></td>`)
	if n := len(f.called(ops.OpUsersList)); n != 0 {
		t.Errorf("a plain user's page asked users_list %d times", n)
	}
}

// D4: an administrator sees the name of the decider from users_list.
func TestAccessGrantsDecidedByShowsTheUserName(t *testing.T) {
	list := grantFixture()
	list.Grants[1].GrantedBy = "u-patryk"
	f := newFakeOps().reply(ops.OpGrantList, list).reply(ops.OpUsersList, ops.UsersListResult{Users: []ops.UserInfo{{ID: "u-patryk", Username: "patryk"}}})
	connect(t, f)
	body := get(t, adminServer(t), pathAccessGrants, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `<td data-label="Decided by">alice <span class="muted">owner</span></td>`,
		`<td data-label="Decided by">patryk <span class="muted">administrator</span></td>`, `<td data-label="Granted" class="grant-level-cell">Control</td>`)
	mustNotHave(t, body, ">u-patryk<")
}

// (e) ?worker= limits the list through grant_list and says so; ?source=
// preselects a live source session and opens the Add form.
func TestAccessGrantsWorkerFilterAndSourcePreset(t *testing.T) {
	f := newFakeOps().reply(ops.OpGrantList, grantFixture())
	connect(t, f)
	d := Dashboard{Workers: []WorkerRow{wPrivate, wInfra}, Sessions: []SessionRow{sAgent, sShell}}
	s, _ := listServer(t, d, d)

	body := get(t, s, pathAccessGrants+"?worker=w1", "X-Test-Session", adminHeader).Body.String()
	calls := f.called(ops.OpGrantList)
	if len(calls) == 0 || calls[len(calls)-1].Args["worker_id"] != "w1" {
		t.Fatalf("grant_list calls = %+v", calls)
	}
	mustHave(t, body, `Showing grants of <strong>laptop</strong> - <a href="/access/grants">show all</a>`,
		`<tbody id="grant-rows" hx-get="/access/grants?worker=w1"`)
	mustNotHave(t, body, "data-grant-open")

	// Without the filter: no line, the list reloads all.
	body = get(t, s, pathAccessGrants, "X-Test-Session", adminHeader).Body.String()
	mustNotHave(t, body, "Showing grants of")
	mustHave(t, body, `<tbody id="grant-rows" hx-get="/access/grants" hx-select`)
	if calls := f.called(ops.OpGrantList); calls[len(calls)-1].Args["worker_id"] != nil {
		t.Errorf("worker_id sent without a filter: %+v", calls[len(calls)-1].Args)
	}

	body = get(t, s, pathAccessGrants+"?source="+url.QueryEscape("w1|fix-tests"), "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `<form id="grant-add-form" class="grant-form" data-grant-open`, `<option value="w1" selected>laptop (alice)</option>`,
		`<option value="w1|fix-tests" data-worker="w1" selected>fix-tests @ laptop</option>`)
	// A user may not start from a Worker of another owner: nothing is preselected.
	body = get(t, s, pathAccessGrants+"?source="+url.QueryEscape("w1|fix-tests"), "X-Test-Session", userHeader).Body.String()
	_, form, _ := strings.Cut(body, `id="grant-add-form"`)
	form, _, _ = strings.Cut(form, "</form>")
	mustNotHave(t, form, "data-grant-open", " selected>")
	// An unknown session is not preselected either.
	body = get(t, s, pathAccessGrants+"?source="+url.QueryEscape("w1|nope"), "X-Test-Session", adminHeader).Body.String()
	mustNotHave(t, body, "data-grant-open")
}

// (f) every row links to its audit history, for administrators only.
func TestAccessGrantsHistoryLinkForAdministrators(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpGrantList, grantFixture()))
	s := adminServer(t)
	admin := get(t, s, pathAccessGrants, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, admin, `<a href="/admin/audit?grant=g1" class="grant-history" id="grant-g1-history">History</a>`,
		`<a href="/admin/audit?grant=g2" class="grant-history" id="grant-g2-history">History</a>`)
	user := get(t, s, pathAccessGrants, "X-Test-Session", userHeader).Body.String()
	mustNotHave(t, user, "/admin/audit", "grant-history")
}
