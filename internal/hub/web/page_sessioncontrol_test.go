package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const (
	scW1 = "0190a1b2-c3d4-7e5f-8a6b-0000000000a1"
	scW2 = "0190a1b2-c3d4-7e5f-8a6b-0000000000a2"
	scW3 = "0190a1b2-c3d4-7e5f-8a6b-0000000000a3"
)

func sampleSessionControl() ops.SessionControlListResult {
	return ops.SessionControlListResult{Grants: []ops.SessionControlInfo{
		{WorkerID: scW1, SessionName: "boss", CreatedBy: "alice", CreatedAt: adminTime, Targets: []ops.KeyResource{
			{Kind: ops.KeyResourceWorker, WorkerID: scW2},
			{Kind: ops.KeyResourceSession, WorkerID: scW3, SessionName: "build"},
		}},
		{WorkerID: scW3, SessionName: "ops", CreatedBy: "alice", CreatedAt: adminTime, Targets: []ops.KeyResource{
			{Kind: ops.KeyResourceEverything},
		}},
	}}
}

func sessionControlServer(t *testing.T) (*Server, *fakeOps) {
	t.Helper()
	f := newFakeOps().reply(ops.OpSessionControlList, sampleSessionControl()).
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{
			{ID: scW1, Name: "W1", Owner: "alice", Online: true},
			{ID: scW2, Name: "W2", Owner: "bob", Online: true},
			{ID: scW3, Name: "W3", Owner: "alice", Online: false},
		}}).
		on(ops.OpSessionsList, func(a map[string]any) (any, error) {
			if a["worker_id"] == scW1 {
				return ops.SessionsListResult{Sessions: []ops.SessionInfo{{Name: "boss"}, {Name: "other"}}}, nil
			}
			return ops.SessionsListResult{Sessions: []ops.SessionInfo{{Name: "x"}}}, nil
		}).
		reply(ops.OpSessionControlSet, delivered)
	connect(t, f)
	return adminServer(t), f
}

func TestSessionControlIsForAdminsOnly(t *testing.T) {
	s, f := sessionControlServer(t)
	if rec := get(t, s, pathAdminSessionControl, "X-Test-Session", userHeader); rec.Code != http.StatusNotFound {
		t.Errorf("GET as user = %d, want 404", rec.Code)
	}
	for _, p := range []string{pathAdminSessionControl, pathAdminSessionControl + "/remove"} {
		form := url.Values{"worker": {scW1}, "session": {"boss"}, "targets": {"all"}}
		if rec := post(t, s, p, userHeader, form); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s as user = %d, want 404", p, rec.Code)
		}
	}
	if f.total() != 0 {
		t.Errorf("a user's request reached the Hub operations: %+v", f.calls)
	}
	user := get(t, s, "/", "X-Test-Session", userHeader).Body.String()
	mustNotHave(t, user, pathAdminSessionControl)
	admin := get(t, s, "/", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, admin, `href="/admin/session-control">`, `<span>Session control</span>`)
}

func TestSessionControlListsGrantsWithRemove(t *testing.T) {
	s, f := sessionControlServer(t)
	rec := get(t, s, pathAdminSessionControl, "X-Test-Session", adminHeader)
	if rec.Code != 200 {
		t.Fatalf("GET = %d\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	mustHave(t, body, "<th scope=\"row\">W1:boss</th>", "<td>Worker W2, W3:build</td>",
		"<th scope=\"row\">W3:ops</th>", "<td>All</td>", "Remove W1:boss", "Remove W3:ops")
	doc := parse(t, body)
	removes := find(doc, func(n *html.Node) bool {
		return n.Data == "form" && attr(n, "hx-post") == pathAdminSessionControl+"/remove"
	})
	if len(removes) != 2 {
		t.Fatalf("%d remove forms, want 2", len(removes))
	}
	hidden := map[string]string{}
	for _, in := range find(removes[0], func(n *html.Node) bool { return n.Data == "input" }) {
		hidden[attr(in, "name")] = attr(in, "value")
	}
	if hidden["worker"] != scW1 || hidden["session"] != "boss" {
		t.Errorf("remove form fields = %v", hidden)
	}
	// Workers of every owner are listed; only online Workers are asked for sessions.
	if c := f.called(ops.OpWorkersList); len(c) != 1 || c[0].Args["all"] != true {
		t.Errorf("workers_list calls = %+v", c)
	}
	if c := f.called(ops.OpSessionsList); len(c) != 2 {
		t.Errorf("sessions_list calls = %+v, want one per online Worker", c)
	}
	mustHave(t, body, `value="worker:`+scW2+`"`, `value="session:`+scW1+`:other"`, `<option value="boss" label="boss on W1 (alice)">`)
	mustNotHave(t, body, `value="session:`+scW3+`:`)
}

func TestSessionControlEmptyListSaysNoGrant(t *testing.T) {
	s, f := sessionControlServer(t)
	f.reply(ops.OpSessionControlList, ops.SessionControlListResult{})
	body := get(t, s, pathAdminSessionControl, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, "There are no grants.")
}

// Default deny: the form opens with no controller, no targets mode and no
// target ticked.
func TestSessionControlFormOpensEmpty(t *testing.T) {
	s, _ := sessionControlServer(t)
	doc := parse(t, get(t, s, pathAdminSessionControl, "X-Test-Session", adminHeader).Body.String())
	forms := find(doc, func(n *html.Node) bool { return n.Data == "form" && attr(n, "hx-post") == pathAdminSessionControl })
	if len(forms) != 1 {
		t.Fatalf("%d grant forms", len(forms))
	}
	for _, n := range find(forms[0], func(n *html.Node) bool {
		return hasAttr(n, "checked") || hasAttr(n, "selected") || (n.Data == "input" && attr(n, "type") == "text" && attr(n, "value") != "")
	}) {
		t.Errorf("preselected control %s %v", n.Data, n.Attr)
	}
	radios := find(forms[0], func(n *html.Node) bool { return n.Data == "input" && attr(n, "type") == "radio" })
	if len(radios) != 2 || attr(radios[0], "value") != "all" || attr(radios[1], "value") != "selected" {
		t.Errorf("targets radios = %d", len(radios))
	}
}

func TestSessionControlSaveAll(t *testing.T) {
	s, f := sessionControlServer(t)
	rec := post(t, s, pathAdminSessionControl, adminHeader, url.Values{
		"worker": {scW1}, "session": {"boss"}, "targets": {"all"}, "target": {"worker:" + scW2},
	})
	if rec.Code != 200 || rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("POST = %d %q", rec.Code, rec.Body)
	}
	c := f.called(ops.OpSessionControlSet)
	if len(c) != 1 {
		t.Fatalf("session_control_set calls = %d", len(c))
	}
	a := c[0].Args
	targets, _ := a["targets"].([]any)
	if a["worker_id"] != scW1 || a["session_name"] != "boss" || len(targets) != 1 ||
		targets[0].(map[string]any)["kind"] != "everything" || !ops.ValidUUIDv7(a["request_id"].(string)) {
		t.Errorf("args = %v", a)
	}
	if fl := flashesOf(rec); len(fl) != 1 || !strings.Contains(fl[0].Text, "boss is saved") {
		t.Errorf("flashes = %+v", fl)
	}
}

// Selected: a ticked Worker covers its own sessions; other sessions stay single.
func TestSessionControlSaveSelected(t *testing.T) {
	s, f := sessionControlServer(t)
	rec := post(t, s, pathAdminSessionControl, adminHeader, url.Values{
		"worker": {scW1}, "session": {"boss"}, "targets": {"selected"},
		"target": {"worker:" + scW2, "session:" + scW2 + ":x", "session:" + scW3 + ":build"},
	})
	if rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("POST = %d %q", rec.Code, rec.Body)
	}
	c := f.called(ops.OpSessionControlSet)
	if len(c) != 1 {
		t.Fatalf("session_control_set calls = %d", len(c))
	}
	got := c[0].Args["targets"].([]any)
	want := []map[string]any{
		{"kind": "worker", "worker_id": scW2},
		{"kind": "session", "worker_id": scW3, "session_name": "build"},
	}
	if len(got) != len(want) {
		t.Fatalf("targets = %v", got)
	}
	for i, w := range want {
		g := got[i].(map[string]any)
		for k, v := range w {
			if g[k] != v {
				t.Errorf("target %d = %v, want %v", i, g, w)
			}
		}
	}
}

// Saving without targets creates nothing: the form answers with a message and
// session_control_set (which would remove a grant) is never called.
func TestSessionControlSaveWithoutTargetsCreatesNothing(t *testing.T) {
	s, f := sessionControlServer(t)
	for name, form := range map[string]url.Values{
		"no mode":          {"worker": {scW1}, "session": {"boss"}, "target": {"worker:" + scW2}},
		"selected nothing": {"worker": {scW1}, "session": {"boss"}, "targets": {"selected"}},
		"no worker":        {"session": {"boss"}, "targets": {"all"}},
		"no session":       {"worker": {scW1}, "targets": {"all"}},
	} {
		rec := post(t, s, pathAdminSessionControl, adminHeader, form)
		if !strings.Contains(rec.Body.String(), `class="form-error"`) || rec.Header().Get("HX-Refresh") != "" {
			t.Errorf("%s: answer %d %q", name, rec.Code, rec.Body)
		}
	}
	if c := f.called(ops.OpSessionControlSet); len(c) != 0 {
		t.Errorf("session_control_set was called: %+v", c)
	}
}

func TestSessionControlRemoveSendsNoTargets(t *testing.T) {
	s, f := sessionControlServer(t)
	rec := post(t, s, pathAdminSessionControl+"/remove", adminHeader, url.Values{"worker": {scW1}, "session": {"boss"}})
	if rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("POST = %d %q", rec.Code, rec.Body)
	}
	c := f.called(ops.OpSessionControlSet)
	if len(c) != 1 {
		t.Fatalf("session_control_set calls = %d", len(c))
	}
	if a := c[0].Args; a["worker_id"] != scW1 || a["session_name"] != "boss" || a["targets"] != nil {
		t.Errorf("args = %v", a)
	}
	if fl := flashesOf(rec); len(fl) != 1 || !strings.Contains(fl[0].Text, "boss is removed") {
		t.Errorf("flashes = %+v", fl)
	}
}

func TestSessionControlShowsTheRefusal(t *testing.T) {
	s, f := sessionControlServer(t)
	f.on(ops.OpSessionControlSet, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 404, Code: "not_found", Message: "not found"}
	})
	rec := post(t, s, pathAdminSessionControl, adminHeader, url.Values{"worker": {scW1}, "session": {"boss"}, "targets": {"all"}})
	if !strings.Contains(rec.Body.String(), `class="form-error"`) {
		t.Errorf("answer %q", rec.Body)
	}
}
