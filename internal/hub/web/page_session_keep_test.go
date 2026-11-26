package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// keepWorker is the Worker of the session view tests announcing session.keep.v1.
func keepWorker() ops.WorkerInfo {
	w := sessionWorker()
	w.Features = []string{proto.FeatureSessionKeepV1}
	return w
}

// The session page offers "Keep after restart" only when the Worker announces
// session.keep.v1 and the person may operate the session, checked while the
// Worker keeps it.
func TestSessionPageKeepCheckboxOnlyWithTheFeature(t *testing.T) {
	s, f := sessionOps(t)
	body := sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String()
	mustNotHave(t, body, `/s/pi-1/minion-1/keep`, `id="keep-session"`)

	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{keepWorker()}})
	body = sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String()
	mustHave(t, body, `hx-post="/s/pi-1/minion-1/keep"`, `hx-trigger="change"`, `<input type="hidden" name="keep" value="0">`,
		`<input type="hidden" name="view" value="session">`,
		`<input type="checkbox" name="keep" value="1" class="keep-box `+keepBoxClass(sessWorkerID, "minion-1")+`" id="keep-session"> Keep after restart`,
		`title="Minion recreates this session with its folders and resumes its Claude/Codex conversation when the Worker starts again"`)

	f.reply(ops.OpSessionsList, ops.SessionsListResult{Sessions: []ops.SessionInfo{{Name: "minion-1", Keep: true}}})
	body = sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String()
	mustHave(t, body, `id="keep-session" checked> Keep after restart`)

	// A viewer who may not operate the session gets no checkbox.
	foreign := keepWorker()
	foreign.OwnerID, foreign.Owner = "u-carol", "carol"
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{foreign}})
	mustNotHave(t, sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String(), `id="keep-session"`)
}

// The form calls session_keep_set as the signed-in user: keep=1 keeps,
// keep=0 alone forgets, a post without keep calls nothing, and a refusal of
// the API is what the person sees.
func TestSessionKeepFormCallsTheOp(t *testing.T) {
	s, f := sessionOps(t)
	f.reply(ops.OpSessionKeepSet, delivered)
	rec := post(t, s, "/s/pi-1/minion-1/keep", sessUser, url.Values{"keep": {"0", "1"}})
	mustHave(t, rec.Body.String(), "<strong>Keep after restart:</strong> Kept. Minion recreates this session")
	if h := rec.Header().Get("HX-Trigger"); h != "" {
		t.Errorf("HX-Trigger = %q, want none", h)
	}
	a := argsOf(f, ops.OpSessionKeepSet, 0)
	tgt, _ := a["target"].(map[string]any)
	if id, _ := a["request_id"].(string); !ops.ValidUUIDv7(id) || a["keep"] != true || tgt["worker_id"] != sessWorkerID || tgt["session_name"] != "minion-1" {
		t.Fatalf("args = %+v", a)
	}

	rec = post(t, s, "/s/pi-1/minion-1/keep", sessUser, url.Values{"keep": {"0"}})
	mustHave(t, rec.Body.String(), "No longer kept.")
	if a := argsOf(f, ops.OpSessionKeepSet, 1); a["keep"] != false {
		t.Fatalf("forget args = %+v", a)
	}

	rec = post(t, s, "/s/pi-1/minion-1/keep", sessUser, url.Values{})
	mustHave(t, rec.Body.String(), "result-danger", "Nothing was changed.")
	if n := len(f.called(ops.OpSessionKeepSet)); n != 2 {
		t.Fatalf("session_keep_set called %d times, want 2", n)
	}

	f.on(ops.OpSessionKeepSet, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 501, Code: "unsupported_op", Message: "the Worker does not implement this operation"}
	})
	rec = post(t, s, "/s/pi-1/minion-1/keep", sessUser, url.Values{"keep": {"0", "1"}})
	mustHave(t, rec.Body.String(), "result-danger", "Keep after restart")
	mustNotHave(t, rec.Body.String(), "Kept.")
}

// The lists offer the checkbox only on the sessions of a Worker that announces
// session.keep.v1, is online and is the person's own (or the person is an
// admin); a kept session elsewhere shows "kept".
func TestHandlerSourceOffersKeepOnlyWhereTheWorkerAnnouncesIt(t *testing.T) {
	feat := []string{proto.FeatureSessionKeepV1}
	api := &fakeAPI{
		workers: ops.WorkersListResult{Workers: []ops.WorkerInfo{
			{ID: "w1", Name: "tv", OwnerID: "u-alice", Online: true, Features: feat},
			{ID: "w2", Name: "old", OwnerID: "u-alice", Online: true},
			{ID: "w3", Name: "bobs", OwnerID: "u-bob", Online: true, Features: feat},
		}},
		sessions: `{"sessions":[{"worker_id":"w1","worker":"tv","session":"a","online":true,"keep":true},
		{"worker_id":"w2","worker":"old","session":"b","online":true},
		{"worker_id":"w3","worker":"bobs","session":"c","online":true,"keep":true}]}`,
	}
	src := NewHandlerSource(NewHandlerCaller(api.handler()))
	user := httptest.NewRequest(http.MethodGet, "/", nil)
	user = user.WithContext(sessionContext(user.Context(), "alice", "user"))
	d, err := src.Snapshot(user, false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][2]bool{}
	for _, x := range d.Sessions {
		got[x.Name] = [2]bool{x.Keep, x.CanKeep}
	}
	if got["a"] != [2]bool{true, true} || got["b"] != [2]bool{false, false} || got["c"] != [2]bool{true, false} {
		t.Fatalf("keep, can keep = %v", got)
	}
	admin := httptest.NewRequest(http.MethodGet, "/", nil)
	admin = admin.WithContext(sessionContext(admin.Context(), "root", "admin"))
	if d, err = src.Snapshot(admin, true); err != nil {
		t.Fatal(err)
	}
	for _, x := range d.Sessions {
		if want := x.WorkerID != "w2"; x.CanKeep != want {
			t.Errorf("admin, %s: can keep %v, want %v", x.Name, x.CanKeep, want)
		}
	}
}

// The session table gets a Keep column only when a session is kept or may be
// kept; the session code stays first in the row and the cell sorts by the flag.
func TestSessionListKeepColumn(t *testing.T) {
	plain := Dashboard{
		Workers:  []WorkerRow{{ID: "w1", Name: "tv", Online: true}},
		Sessions: []SessionRow{{Code: "1-1", WorkerID: "w1", Worker: "tv", Name: "a", Online: true}},
	}
	html, err := renderListFragment("sessions_results", newDashboardView(plain, false))
	if err != nil {
		t.Fatal(err)
	}
	mustNotHave(t, html, `>Keep</th>`, `col-keep`, `name="keep"`)

	d := plain
	d.Sessions = []SessionRow{
		{Code: "1-1", WorkerID: "w1", Worker: "tv", Name: "a", Online: true, Keep: true, CanKeep: true},
		{Code: "1-2", WorkerID: "w1", Worker: "tv", Name: "b", Online: true},
	}
	v := newDashboardView(d, false)
	html, err = renderListFragment("sessions_results", v)
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, html, `<th scope="col" data-sort="text">State</th><th scope="col" data-sort="text" class="col-keep"`,
		`hx-post="/s/tv/a/keep"`, `data-sort-value="1"`, `data-sort-value="0"`, `data-control="keep" checked aria-label="Keep a on tv after restart"`)
	// A114: the code is the first thing of the row.
	row := html[strings.Index(html, `data-session="a"`):]
	if th := row[strings.Index(row, "<th"):]; !strings.HasPrefix(th, `<th scope="row" data-sort-value="tv/a"><code class="session-code"`) {
		t.Errorf("row starts with %.80q", th)
	}
	if strings.Contains(html, `hx-post="/s/tv/b/keep"`) {
		t.Error("a session that may not be kept has a form")
	}

	// The Worker's page and its disclosed list: the same checkbox.
	wv := newWorkerView(d, false, "w1", true)
	html, err = renderWorkerFragment(wv)
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, html, `class="col-keep`, `hx-post="/s/tv/a/keep"`)
	v.discloseSessions()
	html, err = renderListFragment("workers_results", v)
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, html, `hx-post="/s/tv/a/keep"`, `hx-target="#list-result"`)

	// Not changeable, but kept: a badge.
	d.Sessions[0].CanKeep = false
	html, err = renderListFragment("sessions_results", newDashboardView(d, false))
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, html, `data-kept`)
	mustNotHave(t, html, `name="keep"`)
}

// After a refusal the answer sets the checkbox back to the state the Worker
// has (an out-of-band swap of every box of the session), on the session page
// and on a list; a delivered change sets the requested state.
func TestSessionKeepAnswerCarriesTheRealState(t *testing.T) {
	s, f := sessionOps(t)
	class := keepBoxClass(sessWorkerID, "minion-1")
	oob := `hx-swap-oob="outerHTML:.` + class + `"`
	refuse := func(map[string]any) (any, error) {
		return nil, &OpError{Status: 403, Code: "forbidden", Message: "the session is not shared"}
	}

	// Session page: the click wanted keep, the Worker refused and does not keep it.
	f.on(ops.OpSessionKeepSet, refuse)
	f.reply(ops.OpSessionsList, ops.SessionsListResult{Sessions: []ops.SessionInfo{{Name: "minion-1", Keep: false}}})
	body := post(t, s, "/s/pi-1/minion-1/keep", sessUser, url.Values{"view": {"session"}, "keep": {"0", "1"}}).Body.String()
	mustHave(t, body, "result-danger", `<input type="checkbox" name="keep" value="1" class="keep-box `+class+`" id="keep-session" `+oob+`>`)
	mustNotHave(t, body, " checked", `data-control="keep"`)

	// A list: the click wanted to forget, the Worker refused and still keeps it.
	f.reply(ops.OpSessionsList, ops.SessionsListResult{Sessions: []ops.SessionInfo{{Name: "minion-1", Keep: true}}})
	body = post(t, s, "/s/pi-1/minion-1/keep", sessUser, url.Values{"keep": {"0"}}).Body.String()
	mustHave(t, body, "result-danger", `class="keep-box `+class+`" data-control="keep" checked aria-label="Keep minion-1 on pi-1 after restart" `+oob+`>`)
	mustNotHave(t, body, `id="keep-session"`)

	// The state cannot be read: the box goes back to the state before the click.
	f.on(ops.OpSessionsList, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 504, Code: "timeout", Message: "the Worker did not answer"}
	})
	body = post(t, s, "/s/pi-1/minion-1/keep", sessUser, url.Values{"keep": {"0", "1"}}).Body.String()
	mustHave(t, body, "result-danger", oob)
	mustNotHave(t, body, " checked")

	// Delivered: the requested state.
	f.reply(ops.OpSessionKeepSet, delivered)
	body = post(t, s, "/s/pi-1/minion-1/keep", sessUser, url.Values{"keep": {"0", "1"}}).Body.String()
	mustHave(t, body, "Kept.", `data-control="keep" checked`, oob)
}
