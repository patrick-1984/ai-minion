package web

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// shareServer serves the pages over a fake caller that answers the share ops of
// worker w1 ("laptop", alice's, online) the way a Worker would.
func shareServer(t *testing.T, rule ops.ShareResult, mutate func(f *fakeOps)) (*Server, *fakeOps) {
	t.Helper()
	d := Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}}
	f := newFakeOps().
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{wAliceInfo}}).
		reply(ops.OpShareGet, rule).
		reply(ops.OpSessionsList, ops.SessionsListResult{Sessions: []ops.SessionInfo{{Name: "shared-1", Task: "TASK-TEXT-NOT-SHOWN"}, {Name: "shared-2"}}})
	if mutate != nil {
		mutate(f)
	}
	connect(t, f)
	s, _ := listServer(t, d, d)
	return s, f
}

var someRule = ops.ShareResult{Rule: ops.ShareRule{Mode: "select", Names: []string{"shared-1", "shared-2"}}, Unshared: []string{"hidden-1", "hidden<b>"}}

// The panel is names only: the shared sessions checked, the unshared ones listed
// unchecked, the rule said in words; no task, screen or anything else; names are escaped.
func TestSharePanelListsNamesOnlyAndEscapesThem(t *testing.T) {
	s, f := shareServer(t, someRule, nil)
	rec := get(t, s, "/w/w1/share", "X-Test-Session", "alice:user")
	if rec.Code != 200 {
		t.Fatalf("GET = %d", rec.Code)
	}
	body := rec.Body.String()
	mustHave(t, body,
		`action="/w/w1/share"`, `hx-post="/w/w1/share"`, `data-key="share-w1"`,
		`value="shared-1" checked`, `value="shared-2" checked`, `<input type="checkbox" name="name" value="hidden-1">`,
		`value="hidden&lt;b&gt;"`, `(not shared)`, `value="select" data-control="share-select" checked`, `only the chosen sessions are shared`)
	mustNotHave(t, body, "TASK-TEXT-NOT-SHOWN", `hidden<b>`, "<script", "locked")
	if got := f.called(ops.OpShareGet); len(got) != 1 || got[0].Args["worker_id"] != "w1" || got[0].User != "alice" {
		t.Fatalf("share_get calls = %+v (the call is made as the signed-in user, on the canonical id)", got)
	}
	if len(f.called(ops.OpShareSet)) != 0 {
		t.Fatal("the GET changed something")
	}
}

func TestSharePanelSaysWhyItCannotBeUsed(t *testing.T) {
	// Not the owner: the Worker is in alice's list only as a Worker she may see, here bob's.
	t.Run("not allowed", func(t *testing.T) {
		d := Dashboard{Workers: []WorkerRow{wPrivate}}
		f := newFakeOps().reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{wBobInfo}})
		connect(t, f)
		s, _ := listServer(t, d, d)
		rec := get(t, s, "/w/w2/share", "X-Test-Session", "alice:user")
		mustHave(t, rec.Body.String(), "Only the owner of this worker, or an administrator")
		mustNotHave(t, rec.Body.String(), "<form", `name="mode"`)
		if len(f.called(ops.OpShareGet)) != 0 {
			t.Fatal("share_get was called for a person who may not operate the Worker")
		}
	})
	t.Run("offline", func(t *testing.T) {
		w := wAliceInfo
		w.Online = false
		d := Dashboard{Workers: []WorkerRow{wPrivate}}
		f := newFakeOps().reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{w}})
		connect(t, f)
		s, _ := listServer(t, d, d)
		rec := get(t, s, "/w/w1/share", "X-Test-Session", "alice:user")
		mustHave(t, rec.Body.String(), "This worker is offline")
		if len(f.called(ops.OpShareGet)) != 0 {
			t.Fatal("share_get was called for an offline Worker")
		}
	})
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"preview only":  {&OpError{Status: 403, Code: "forbidden", Message: "the Worker is preview-only"}, "the Worker is preview-only"},
		"old worker":    {&OpError{Status: 501, Code: "unsupported_op", Message: "x"}, "too old to change its sharing from the Hub"},
		"lost link":     {&OpError{Status: 503, Code: "worker_offline", Message: "x"}, "This worker is offline"},
		"internal fail": {&OpError{Status: 500, Code: "internal", Message: "boom"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := shareServer(t, someRule, func(f *fakeOps) {
				f.on(ops.OpShareGet, func(map[string]any) (any, error) { return nil, tc.err })
			})
			rec := get(t, s, "/w/w1/share", "X-Test-Session", "alice:user")
			if rec.Code != 200 {
				t.Fatalf("GET = %d", rec.Code)
			}
			mustHave(t, rec.Body.String(), `data-share-note`, tc.want)
			mustNotHave(t, rec.Body.String(), `<form`, `name="mode"`)
		})
	}
	t.Run("locked", func(t *testing.T) {
		locked := someRule
		locked.Locked = true
		s, _ := shareServer(t, locked, nil)
		rec := get(t, s, "/w/w1/share", "X-Test-Session", "alice:user")
		mustHave(t, rec.Body.String(), "locked on the worker (share_lock)", `<fieldset disabled`, `data-control="share-apply" disabled`)
	})
}

func TestSharePanelIsA404ForAWorkerOutsideTheCallersList(t *testing.T) {
	s, f := shareServer(t, someRule, nil)
	for _, p := range []string{"/w/nope/share", "/w/w2/share"} {
		if rec := get(t, s, p, "X-Test-Session", "alice:user"); rec.Code != 404 {
			t.Fatalf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	rec := post(t, s, "/w/nope/share", "alice:user", url.Values{"mode": {"none"}})
	mustNotHave(t, rec.Body.String(), "Saved", "hidden-1")
	if len(f.called(ops.OpShareGet)) != 0 || len(f.called(ops.OpShareSet)) != 0 {
		t.Fatal("a Worker outside the list was asked")
	}
}

// The form posts exactly the chosen rule: all and none carry no names, select
// carries the checked names once each and sorted; the answer is the refreshed panel with
// the rule that is now in force.
func TestSharePostCallsShareSetWithTheChosenRule(t *testing.T) {
	for name, tc := range map[string]struct {
		form url.Values
		want map[string]any
		said string
	}{
		"all":  {url.Values{"mode": {"all"}, "name": {"x"}}, map[string]any{"worker_id": "w1", "mode": "all"}, "shares all its sessions"},
		"none": {url.Values{"mode": {"none"}}, map[string]any{"worker_id": "w1", "mode": "none"}, "shares no session"},
		"select": {url.Values{"mode": {"select"}, "name": {"b", "a", "b"}},
			map[string]any{"worker_id": "w1", "mode": "select", "names": []any{"a", "b"}}, "only the sessions you chose"},
	} {
		t.Run(name, func(t *testing.T) {
			s, f := shareServer(t, someRule, func(f *fakeOps) {
				f.on(ops.OpShareSet, func(args map[string]any) (any, error) {
					return ops.ShareResult{Rule: ops.ShareRule{Mode: args["mode"].(string)}}, nil
				})
			})
			rec := post(t, s, "/w/w1/share", "alice:user", tc.form)
			if rec.Code != 200 {
				t.Fatalf("POST = %d", rec.Code)
			}
			mustHave(t, rec.Body.String(), `class="result result-info"`, "<strong>Sharing:</strong> Saved.", tc.said, `data-key="share-w1"`)
			calls := f.called(ops.OpShareSet)
			if len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, tc.want) || calls[0].User != "alice" {
				t.Fatalf("share_set calls = %+v, want %v", calls, tc.want)
			}
		})
	}
}

// A refusal is shown as the refusal it is, and nothing is claimed saved; a select without names
// is refused by the API, and the form says what to do.
func TestSharePostShowsRefusalsAndNeverClaimsASave(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		form url.Values
		want string
	}{
		"locked":    {&OpError{Status: 403, Code: "forbidden", Message: "the share selection of this Worker is locked locally (share_lock)"}, url.Values{"mode": {"none"}}, "share_lock"},
		"preview":   {&OpError{Status: 403, Code: "forbidden", Message: "the Worker is preview-only"}, url.Values{"mode": {"none"}}, "preview-only"},
		"no names":  {&OpError{Status: 400, Code: "invalid_argument", Message: "names: select needs at least one session name", Field: "names"}, url.Values{"mode": {"select"}}, "cannot be applied: names: select needs at least one"},
		"foreign":   {&OpError{Status: 404, Code: "not_found", Message: "not found"}, url.Values{"mode": {"none"}}, ""},
		"went away": {&OpError{Status: 503, Code: "worker_offline", Message: "x"}, url.Values{"mode": {"all"}}, "This worker is offline"},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := shareServer(t, someRule, func(f *fakeOps) {
				f.on(ops.OpShareSet, func(map[string]any) (any, error) { return nil, tc.err })
			})
			rec := post(t, s, "/w/w1/share", "alice:user", tc.form)
			body := rec.Body.String()
			mustHave(t, body, `class="result result-danger"`, tc.want)
			mustNotHave(t, body, "Saved")
		})
	}
}

// The Worker page offers the panel at the Worker's canonical id to the person who may operate it,
// outside the live fragment (a half-made choice must not be replaced by an update), and to nobody else.
func TestWorkerPageOffersTheSharePanelOutsideTheLiveFragment(t *testing.T) {
	d := Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}}
	s, _ := workerPageServer(t, d, d, wAliceInfo)
	_, own := page(t, s, "/workers/w1", "alice:user")
	mustHave(t, own, `id="share-panel"`, `hx-get="/w/w1/share"`, `hx-trigger="load"`)
	start := strings.Index(own, `id="results"`)
	end := strings.Index(own, `id="share-panel"`)
	if start < 0 || end < start || !strings.Contains(own[start:end], "</div>") {
		t.Fatalf("the panel is not after the live results container")
	}
	// The live fragment is unchanged: it carries no share form, and keeps its stable focus keys.
	frag, err := renderWorkerFragment(newWorkerView(d, false, "w1", true))
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, frag, `data-key="worker-actions-w1"`, `data-key="worker-sessions-w1"`)
	mustNotHave(t, frag, "share")

	// A user who may see the Worker but not operate it gets no panel link.
	shared := Dashboard{Workers: []WorkerRow{wPrivate}}
	s2, _ := workerPageServer(t, shared, shared, func() ops.WorkerInfo { w := wAliceInfo; w.OwnerID = "u-other"; return w }())
	_, other := page(t, s2, "/workers/w1", "alice:user")
	mustNotHave(t, other, `id="share-panel"`, `/w/w1/share`)
	// An administrator operates every Worker.
	_, adm := page(t, s2, "/workers/w1?scope=all", "root:admin")
	mustHave(t, adm, `hx-get="/w/w1/share"`)
}
