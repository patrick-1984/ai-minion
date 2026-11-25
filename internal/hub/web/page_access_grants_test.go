package web

import (
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
			TargetSession: "wb-3", Level: ops.GrantLevelView, Authority: ops.GrantAuthorityOwner, GrantedBy: "alice", Revision: 3,
			SourceWorkerName: "work<b>", TargetWorkerName: "coder", SourceOwner: "alice", TargetOwner: "alice", State: ops.GrantStateActive},
		{ID: "g2", SourceWorkerID: "wA", SourceSession: "ci", SourceProof: ops.GrantProofVerified, TargetWorkerID: "wC",
			Level: ops.GrantLevelControl, Authority: ops.GrantAuthorityAdmin, GrantedBy: "root", Revision: 7, Suspended: true,
			SourceWorkerName: "work<b>", TargetWorkerName: "anna-laptop", SourceOwner: "alice", TargetOwner: "anna", State: ops.GrantStateSuspended},
	}}
}

func TestAccessGrantsPageListsGrantsForEveryUser(t *testing.T) {
	f := newFakeOps().reply(ops.OpGrantList, grantFixture())
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
		`>Verified</span>`, "all shared sessions of anna-laptop", `<span class="badge" data-tone="positive">In effect</span>`,
		`<span class="badge" data-tone="danger">Suspended: needs an administrator</span>`,
		`alice <span class="muted">owner</span>`, `root <span class="muted">administrator</span>`, `<td data-label="Until">no end</td>`,
		`<tr id="grant-g2" data-filter="ci work&lt;b&gt; alice  anna-laptop anna control root" data-between="1" data-off="1">`,
		`hx-post="/access/grants/set-level" hx-target="closest tr" hx-swap="outerHTML" hx-sync="closest tr:replace" hx-disabled-elt="find button"`,
		`name="if_revision" value="7"`, `<option value="message">Message</option>`,
		`hx-post="/access/grants/approve"`, `hx-post="/access/grants/revoke"`,
		`<tbody id="grant-rows" hx-get="/access/grants" hx-select="#grant-rows" hx-swap="outerHTML" hx-trigger="grants-changed from:body">`,
		`<dialog id="grant-dialog"`, `<details class="panel grant-add" id="grant-add">`, `id="grant-only-between"`, `id="grant-only-off"`,
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
	mustHave(t, body, `<p class="grant-status" role="status">Revoked</p>`)

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
		"target_worker_id": {"wB"}, "target_session": {"wb-3"}}
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
	mustHave(t, body, `<tr id="grant-g1"`, `<p class="grant-status grant-error" role="alert">Changed by someone else. This is the current state.</p>`,
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
		`<option value="">every shared session of this Worker</option>`, `<input type="radio" name="level" value="message">`,
		`name="until" value="30"`, `hx-post="/access/grants/add" hx-target="#grant-add-result"`)
	user := get(t, s, pathAccessGrants, "X-Test-Session", userHeader).Body.String()
	_, sources, _ := strings.Cut(user, `name="source_worker_id"`)
	sources, _, _ = strings.Cut(sources, `name="target_worker_id"`)
	mustHave(t, sources, `<option value="w2">buildbox (bob)</option>`)
	mustNotHave(t, sources, `value="w1"`)

	before := time.Now().UTC()
	rec := post(t, s, pathAccessGrants+"/add", adminHeader, url.Values{"source_worker_id": {"w1"}, "source_session": {"w1|fix-tests"},
		"target_worker_id": {"w2"}, "target_session": {""}, "level": {"control"}, "until": {"7"}, "note": {" for the build "}})
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
		"target_worker_id": {"w1"}, "target_session": {"w1|fix-tests"}, "level": {"view"}})
	mustHave(t, rec.Body.String(), `<p class="form-error" role="alert">you do not own that Worker</p>`)
	if rec.Header().Get("HX-Trigger") != "" {
		t.Error("a refused grant reloads the list")
	}
	rec = post(t, s, pathAccessGrants+"/add", userHeader, url.Values{"source_worker_id": {"w2"}, "source_session": {"w1|fix-tests"},
		"target_worker_id": {"w1"}, "level": {"view"}})
	mustHave(t, rec.Body.String(), "The source session is not on the chosen source Worker.")
	if len(f.called(ops.OpGrantSet)) != 1 {
		t.Error("a mismatched session reached the Hub")
	}
}
