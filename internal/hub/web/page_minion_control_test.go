package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func controlFixture() ops.ControlListResult {
	return ops.ControlListResult{Workers: []ops.ControlWorker{
		{WorkerID: "wA", Name: "boss-box", Owner: "root", Online: true, UserManaged: true, CanBeMaster: true,
			Masters: []ops.ControlMaster{{SessionName: "boss"}}, Accepts: []ops.ControlAccept{}},
		{WorkerID: "wB", Name: "ci", Owner: "root", Online: true, UserManaged: true, CanBeMaster: true,
			Masters: []ops.ControlMaster{}, Accepts: []ops.ControlAccept{{SourceWorkerID: "wA"}}},
		{WorkerID: "wT", Name: "esp32", Owner: "root", UserManaged: true, CanBeMaster: false,
			Masters: []ops.ControlMaster{}, Accepts: []ops.ControlAccept{}},
	}}
}

func TestMinionControlPageShowsTheModelAndIsForAdminsOnly(t *testing.T) {
	f := newFakeOps().reply(ops.OpControlList, controlFixture())
	connect(t, f)
	s := adminServer(t)
	if rec := get(t, s, pathMinionControl, "X-Test-Session", userHeader); rec.Code != http.StatusNotFound {
		t.Fatalf("user GET = %d", rec.Code)
	}
	if rec := post(t, s, pathMinionControl+"/master", userHeader, url.Values{"worker_id": {"wA"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("user POST = %d", rec.Code)
	}
	body := get(t, s, pathMinionControl, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, "master session", "slave session", `id="ctl-wA"`,
		// ci accepts boss-box: ticked in the matrix and in ci's picker.
		`value="on" checked aria-label="ci accepts control from boss-box"`,
		// a thin Worker cannot be a master
		"cannot be a master (thin Worker or older than 0.8.0)",
		`<script src="/static/minion-control.js" defer></script>`,
		// the page uses the window, shows every Worker row, and the matrix names the owner
		`class="page-head access-wide"`, `class="table-wrap table-wrap-full"`,
		`<th scope="row">boss-box <span class="muted matrix-owner">root</span></th>`,
		`As of <time datetime="`, `hx-get="/access/control" hx-target="#ctl-matrix" hx-disabled-elt="this"`,
		`<span class="refresh-idle">Refresh</span>`)
	css, err := staticFiles.ReadFile("static/minion.css")
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, string(css), `main:has(> .access-wide) { max-width: none; }`, `.table-wrap.table-wrap-full { max-height: none; }`,
		`main:has(> .access-wide) input[type="checkbox"] { width: 24px; height: 24px;`)
}

// One click is one call; the answer is the row as the Hub has it now, with
// an Undo that posts the inverse.
func TestMinionControlClickSavesAndOffersUndo(t *testing.T) {
	f := newFakeOps().reply(ops.OpControlList, controlFixture()).reply(ops.OpControlSetWorker, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	rec := post(t, adminServer(t), pathMinionControl+"/user-managed", adminHeader, url.Values{"worker_id": {"wB"}})
	if rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	calls := f.called(ops.OpControlSetWorker)
	if len(calls) != 1 || calls[0].Args["worker_id"] != "wB" || calls[0].Args["user_managed"] != false {
		t.Fatalf("calls = %+v (an unticked box is off)", calls)
	}
	if rec.Header().Get("HX-Trigger") != "control-changed" {
		t.Error("the matrix is not told to refresh")
	}
	mustHave(t, rec.Body.String(), `id="ctl-wB"`, "Only master sessions control this Worker now.",
		`hx-post="/access/control/user-managed"`, `name="value" value="on"`, ">Undo</button>")
}

func TestMinionControlRefusalShowsTheHubsState(t *testing.T) {
	f := newFakeOps().reply(ops.OpControlList, controlFixture()).on(ops.OpControlSetMaster, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 409, Code: "conflict", Message: "this Worker cannot control others"}
	})
	connect(t, f)
	rec := post(t, adminServer(t), pathMinionControl+"/master", adminHeader, url.Values{"worker_id": {"wT"}, "session_name": {"x"}, "value": {"on"}})
	mustHave(t, rec.Body.String(), `id="ctl-wT"`, "this Worker cannot control others")
	mustNotHave(t, rec.Body.String(), "Undo")
}

// Removing a migrated (limited) master offers no one-click undo: re-adding
// would drop its old limit.
func TestRemovingALimitedMasterHasNoUndo(t *testing.T) {
	f := newFakeOps().reply(ops.OpControlList, controlFixture()).reply(ops.OpControlSetMaster, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	rec := post(t, adminServer(t), pathMinionControl+"/master", adminHeader, url.Values{"worker_id": {"wA"}, "session_name": {"boss"}, "legacy": {"1"}})
	mustHave(t, rec.Body.String(), "without its old limit")
	mustNotHave(t, rec.Body.String(), ">Undo</button>")
}

// Removing an acceptance limited to some sessions offers no Undo: re-adding
// would accept every shared session. An unlimited one keeps its Undo.
func TestRemovingALimitedAcceptanceHasNoUndo(t *testing.T) {
	fx := controlFixture()
	ci := []string{"ci"}
	fx.Workers[1].Accepts = []ops.ControlAccept{{SourceWorkerID: "wA", Sessions: &ci}}
	f := newFakeOps().reply(ops.OpControlList, fx).reply(ops.OpControlSetAccept, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	rec := post(t, adminServer(t), pathMinionControl+"/accept", adminHeader, url.Values{"worker_id": {"wB"}, "source_worker_id": {"wA"}})
	mustHave(t, rec.Body.String(), "limited to some sessions")
	mustNotHave(t, rec.Body.String(), ">Undo</button>")

	f = newFakeOps().reply(ops.OpControlList, controlFixture()).reply(ops.OpControlSetAccept, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	rec = post(t, adminServer(t), pathMinionControl+"/accept", adminHeader, url.Values{"worker_id": {"wB"}, "source_worker_id": {"wA"}})
	mustHave(t, rec.Body.String(), ">Undo</button>")
}

// A19: no free-text session name; a Worker without shared sessions says so,
// and the check takes one "worker|session" choice.
func TestMinionControlOffersOnlyKnownSessionsAndOneCheckChoice(t *testing.T) {
	f := newFakeOps().reply(ops.OpControlList, controlFixture()).reply(ops.OpControlCheck, ops.ControlCheckResult{
		Controllers: []ops.ControlChain{{WorkerID: "wA", Worker: "boss-box", SessionName: "boss"}}})
	connect(t, f)
	body := get(t, adminServer(t), pathMinionControl+"?check=wB%7Cci-1", "X-Test-Session", adminHeader).Body.String()
	mustNotHave(t, body, `placeholder="session name"`, `name="check_session"`)
	mustHave(t, body, "No shared session on this Worker", "can be controlled by 1 master session:", "<li>boss-box / boss</li>", `class="corner"`)
	calls := f.called(ops.OpControlCheck)
	if len(calls) != 1 || calls[0].Args["worker_id"] != "wB" || calls[0].Args["session_name"] != "ci-1" {
		t.Fatalf("check calls = %+v", calls)
	}
}

// When every shared session is already a master, the picker says so instead
// of asking to share sessions.
func TestMinionControlAllSharedSessionsAreMasters(t *testing.T) {
	f := newFakeOps().reply(ops.OpControlList, controlFixture())
	connect(t, f)
	body := get(t, adminServer(t), pathMinionControl, "X-Test-Session", adminHeader).Body.String()
	// boss-box (online, one master, no other known session).
	start := strings.Index(body, `id="ctl-wA"`)
	end := strings.Index(body[start:], "</tr>")
	row := body[start : start+end]
	mustHave(t, row, "No other shared session to nominate.")
	mustNotHave(t, row, "No shared session on this Worker")
}

// An acceptance left with no session (they all ended) allows nothing; its
// removal offers no Undo, which would re-add it for every shared session.
func TestRemovingAnEmptyAcceptanceHasNoUndo(t *testing.T) {
	fx := controlFixture()
	none := []string{}
	fx.Workers[1].Accepts = []ops.ControlAccept{{SourceWorkerID: "wA", Sessions: &none}}
	b, _ := json.Marshal(fx.Workers[1].Accepts[0])
	if string(b) != `{"source_worker_id":"wA","sessions":[]}` {
		t.Fatalf("JSON = %s", b)
	}
	f := newFakeOps().reply(ops.OpControlList, fx).reply(ops.OpControlSetAccept, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	rec := post(t, adminServer(t), pathMinionControl+"/accept", adminHeader, url.Values{"worker_id": {"wB"}, "source_worker_id": {"wA"}})
	mustNotHave(t, rec.Body.String(), ">Undo</button>")
}

// acceptStatesFixture: ci accepts two sessions from boss-box, esp32 has an
// explicit-nothing row for boss-box.
func acceptStatesFixture() ops.ControlListResult {
	fx := controlFixture()
	two, none := []string{"ci", "ci-2"}, []string{}
	fx.Workers[1].Accepts = []ops.ControlAccept{{SourceWorkerID: "wA", Sessions: &two}}
	fx.Workers[2].Accepts = []ops.ControlAccept{{SourceWorkerID: "wA", Sessions: &none}}
	return fx
}

const (
	limitedBadge = `<span class="badge" data-tone="warning" title="ci accepts only 2 sessions from boss-box"><span aria-hidden="true">limited (2)</span><span class="sr-only">ci accepts only 2 sessions from boss-box</span></span>`
	blockedBadge = `<span class="badge" data-tone="danger" title="esp32 accepts nothing from boss-box"><span aria-hidden="true">blocked</span><span class="sr-only">esp32 accepts nothing from boss-box</span></span>`
)

// A limited acceptance and an explicit-nothing one are not a plain tick: the
// picker and the matrix mark them, and the explicit-nothing one is not ticked.
func TestMinionControlShowsLimitedAndBlockedAcceptances(t *testing.T) {
	f := newFakeOps().reply(ops.OpControlList, acceptStatesFixture())
	connect(t, f)
	body := get(t, adminServer(t), pathMinionControl, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body,
		// picker
		`value="on" checked> boss-box <span class="muted">root</span></label> `+limitedBadge,
		`value="on"> boss-box <span class="muted">root</span></label> `+blockedBadge,
		// matrix
		`value="on" checked aria-label="ci accepts control from boss-box"> `+limitedBadge,
		`value="on" aria-label="esp32 accepts control from boss-box"> `+blockedBadge)
	mustNotHave(t, body, `checked aria-label="esp32 accepts control from boss-box"`)
	start := strings.Index(body, `id="ctl-wT"`)
	mustHave(t, body[start:start+strings.Index(body[start:], "</tr>")], "0 selected")

	// "All Workers" on: the matrix shows the mark instead of "all", and the
	// row names the exceptions (its picker is hidden).
	fx := acceptStatesFixture()
	fx.Workers[0].AcceptAll, fx.Workers[1].AcceptAll, fx.Workers[2].AcceptAll = true, true, true
	fx.Workers[1].Masters = []ops.ControlMaster{{SessionName: "m"}}
	f = newFakeOps().reply(ops.OpControlList, fx)
	connect(t, f)
	body = get(t, adminServer(t), pathMinionControl, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `<td>`+limitedBadge+`</td>`, `<td>`+blockedBadge+`</td>`,
		`Except: boss-box `+limitedBadge, `Except: boss-box `+blockedBadge,
		// no row at all: still the plain "all"
		`<span class="badge plain" title="boss-box accepts all Workers">all</span>`,
		`<span class="badge plain" title="esp32 accepts all Workers">all</span>`)
	if n := strings.Count(body, `accepts all Workers">all</span>`); n != 2 {
		t.Errorf("plain all cells = %d, want 2 (boss-box and esp32 from ci)", n)
	}
}

func postAccept(t *testing.T, fx ops.ControlListResult, target, value string) (*fakeOps, string) {
	t.Helper()
	f := newFakeOps().reply(ops.OpControlList, fx).reply(ops.OpControlSetAccept, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	form := url.Values{"worker_id": {target}, "source_worker_id": {"wA"}}
	if value != "" {
		form.Set("value", value)
	}
	rec := post(t, adminServer(t), pathMinionControl+"/accept", adminHeader, form)
	if rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	return f, rec.Body.String()
}

// A tick on a limited acceptance never widens it and writes nothing (a list
// read a moment ago may already be stale): the row says where to change it.
func TestTickOnALimitedAcceptanceKeepsItsList(t *testing.T) {
	f, body := postAccept(t, acceptStatesFixture(), "wB", "on")
	if calls := f.called(ops.OpControlSetAccept); len(calls) != 0 {
		t.Fatalf("calls = %+v, want no write", calls)
	}
	mustHave(t, body, `id="ctl-wB"`, "This acceptance is limited to 2 sessions. Change the list with the operation control_set_accept.", limitedBadge)
	mustNotHave(t, body, ">Undo</button>", "Saved:")
}

// Every form sends back what the page showed, and the click carries it as the
// precondition of the write; Undo states what the change left stored.
func TestControlClicksCarryWhatThePageShowed(t *testing.T) {
	f := newFakeOps().reply(ops.OpControlList, acceptStatesFixture())
	connect(t, f)
	body := get(t, adminServer(t), pathMinionControl, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `name="if_state" value="limited"`, `name="if_state" value="none"`, `name="if_state" value="absent"`,
		`name="if_value" value="on"`, `name="if_value" value="off"`, `name="if_master" value="on"`)

	ok := ops.WriteResult{Outcome: ops.OutcomeDelivered}
	f = newFakeOps().reply(ops.OpControlList, controlFixture()).reply(ops.OpControlSetAccept, ok).reply(ops.OpControlSetMaster, ok).reply(ops.OpControlSetWorker, ok)
	connect(t, f)
	s := adminServer(t)
	rec := post(t, s, pathMinionControl+"/accept", adminHeader, url.Values{"worker_id": {"wT"}, "source_worker_id": {"wA"}, "value": {"on"}, "if_state": {"absent"}})
	if c := f.called(ops.OpControlSetAccept); len(c) != 1 || c[0].Args["if_state"] != "absent" {
		t.Fatalf("accept calls = %+v", c)
	}
	mustHave(t, rec.Body.String(), ">Undo</button>", `name="if_state" value="all"`)
	// A form without the member (a page from before the update) is checked
	// against what the Hub had when the click arrived.
	post(t, s, pathMinionControl+"/accept", adminHeader, url.Values{"worker_id": {"wB"}, "source_worker_id": {"wA"}})
	if c := f.called(ops.OpControlSetAccept); len(c) != 2 || c[1].Args["if_state"] != "all" {
		t.Fatalf("accept calls = %+v", c)
	}
	rec = post(t, s, pathMinionControl+"/master", adminHeader, url.Values{"worker_id": {"wA"}, "session_name": {"boss"}, "if_master": {"on"}})
	if c := f.called(ops.OpControlSetMaster); len(c) != 1 || c[0].Args["if_master"] != true || c[0].Args["master"] != false {
		t.Fatalf("master calls = %+v", c)
	}
	mustHave(t, rec.Body.String(), ">Undo</button>", `name="if_master" value="off"`)
	rec = post(t, s, pathMinionControl+"/accept-all", adminHeader, url.Values{"worker_id": {"wB"}, "value": {"on"}, "if_value": {"off"}})
	if c := f.called(ops.OpControlSetWorker); len(c) != 1 || c[0].Args["if_accept_all"] != false || c[0].Args["accept_all"] != true {
		t.Fatalf("worker calls = %+v", c)
	}
	mustHave(t, rec.Body.String(), ">Undo</button>", `name="if_value" value="on"`)
}

// A click on a state another administrator changed is refused by the Hub: the
// row shows the conflict and what the Hub has now, without an Undo.
func TestAcceptConflictShowsTheHubsRow(t *testing.T) {
	f := newFakeOps().reply(ops.OpControlList, acceptStatesFixture()).on(ops.OpControlSetAccept, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 409, Code: "conflict", Message: "changed by someone else meanwhile; nothing was saved"}
	})
	connect(t, f)
	// The page showed no entry for ci; meanwhile it was limited to two sessions.
	rec := post(t, adminServer(t), pathMinionControl+"/accept", adminHeader,
		url.Values{"worker_id": {"wB"}, "source_worker_id": {"wA"}, "value": {"on"}, "if_state": {"absent"}})
	calls := f.called(ops.OpControlSetAccept)
	if len(calls) != 1 || calls[0].Args["if_state"] != "absent" {
		t.Fatalf("calls = %+v, want the Hub to decide on the state the page showed", calls)
	}
	if _, has := calls[0].Args["sessions"]; has {
		t.Fatalf("a stale list was sent: %+v", calls[0].Args)
	}
	body := rec.Body.String()
	mustHave(t, body, `id="ctl-wB"`, `role="alert">changed by someone else meanwhile`, limitedBadge, `name="if_state" value="limited"`)
	mustNotHave(t, body, ">Undo</button>", "Saved:")
}

// A tick on an explicit-nothing row grants the whole Worker and says so. Undo
// only where a revoke writes the explicit-nothing row back (accept_all on).
func TestTickOnABlockedAcceptanceGrantsTheWholeWorker(t *testing.T) {
	f, body := postAccept(t, acceptStatesFixture(), "wT", "on")
	calls := f.called(ops.OpControlSetAccept)
	if len(calls) != 1 || calls[0].Args["accept"] != true {
		t.Fatalf("calls = %+v", calls)
	}
	if _, has := calls[0].Args["sessions"]; has {
		t.Fatalf("a whole-Worker grant carries no list: %+v", calls[0].Args)
	}
	mustHave(t, body, "esp32 now accepts every shared session from boss-box.", "No Undo")
	mustNotHave(t, body, ">Undo</button>")

	fx := acceptStatesFixture()
	fx.Workers[2].AcceptAll = true
	_, body = postAccept(t, fx, "wT", "on")
	mustHave(t, body, "esp32 now accepts every shared session from boss-box.", ">Undo</button>", `name="value" value="off"`)
}

// Undo is offered only where the inverse click restores the stored state.
func TestAcceptUndoRestoresExactlyOrIsNotOffered(t *testing.T) {
	// no row -> accepted: the untick deletes the row again.
	f, body := postAccept(t, controlFixture(), "wT", "on")
	if _, has := f.called(ops.OpControlSetAccept)[0].Args["sessions"]; has {
		t.Fatal("a plain tick carries no list")
	}
	mustHave(t, body, "Accepts control from that Worker.", ">Undo</button>", `name="value" value="off"`)
	// no row on a Worker that accepts all: the untick would block the source.
	fx := controlFixture()
	fx.Workers[2].AcceptAll = true
	_, body = postAccept(t, fx, "wT", "on")
	mustHave(t, body, "No Undo: it would block that Worker.")
	mustNotHave(t, body, ">Undo</button>")
	// whole Worker -> removed: the tick accepts the whole Worker again.
	_, body = postAccept(t, controlFixture(), "wB", "")
	mustHave(t, body, "No longer accepts control from that Worker.", ">Undo</button>", `name="value" value="on"`)
	// limited -> removed, blocked -> removed, nothing -> removed: no Undo.
	for target, want := range map[string]string{"wB": "so there is no Undo", "wT": "No Undo: it accepted nothing from it before."} {
		_, body = postAccept(t, acceptStatesFixture(), target, "")
		mustHave(t, body, want)
		mustNotHave(t, body, ">Undo</button>")
	}
	_, body = postAccept(t, controlFixture(), "wT", "")
	mustNotHave(t, body, ">Undo</button>")
}

// The control page fits a phone (D6): the matrix box, not the page, holds the
// hidden words of the badges in its cells.
func TestMinionControlMatrixScrollsOnlyInsideItsBox(t *testing.T) {
	rules := loadSheet(t)
	wrap := el("div", "matrix-wrap")
	for _, w := range []int{390, 1440} {
		if got := valueAt(rules, "position", wrap, nil, w); got != "relative" {
			t.Errorf("position of .matrix-wrap at %d px = %q, want relative", w, got)
		}
		if got := valueAt(rules, "overflow", wrap, nil, w); got != "auto" {
			t.Errorf("overflow of .matrix-wrap at %d px = %q, want auto", w, got)
		}
	}
}

// A check the Hub cannot answer is shown as failed, never as "no master
// session".
func TestMinionControlShowsAFailedCheck(t *testing.T) {
	f := newFakeOps().reply(ops.OpControlList, controlFixture()).on(ops.OpControlCheck, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 500, Code: "internal", Message: "store unavailable"}
	})
	connect(t, f)
	body := get(t, adminServer(t), pathMinionControl+"?check=wB%7Cci-1", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, "the check failed, so it is not known who can control this session")
	mustNotHave(t, body, "no master session.")
}

// v0.25.0: an acceptance of the whole Worker has an apps_install box (admins
// only); a click sets only the flag, keeps the acceptance and offers Undo. A
// limited acceptance is not widened: the click writes nothing.
func TestMinionControlAppsInstallBox(t *testing.T) {
	f := newFakeOps().reply(ops.OpControlList, controlFixture())
	connect(t, f)
	s := adminServer(t)
	body := get(t, s, pathMinionControl, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `hx-post="/access/control/apps-install"`, `aria-label="Master sessions of boss-box may install apps on ci"> may install apps (apps_install)`)
	if rec := post(t, s, pathMinionControl+"/apps-install", userHeader, url.Values{"worker_id": {"wB"}, "source_worker_id": {"wA"}, "value": {"on"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("user POST = %d", rec.Code)
	}

	fx := controlFixture()
	f = newFakeOps().reply(ops.OpControlList, fx).reply(ops.OpControlSetAccept, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	rec := post(t, s, pathMinionControl+"/apps-install", adminHeader, url.Values{"worker_id": {"wB"}, "source_worker_id": {"wA"}, "value": {"on"}, "if_state": {"all"}})
	calls := f.called(ops.OpControlSetAccept)
	if len(calls) != 1 || calls[0].Args["apps_install"] != true || calls[0].Args["accept"] != true || calls[0].Args["if_state"] != "all" {
		t.Fatalf("calls = %+v", calls)
	}
	if _, has := calls[0].Args["sessions"]; has {
		t.Fatalf("the flag carried a session list: %+v", calls[0].Args)
	}
	if _, has := calls[0].Args["files_push"]; has {
		t.Fatalf("the flag changed a file flag: %+v", calls[0].Args)
	}
	mustHave(t, rec.Body.String(), `id="ctl-wB"`, "Master sessions of boss-box may install apps on ci", "confirmation on the device",
		">Undo</button>", `name="value" value="off"`)

	// Unticked: the flag goes off.
	post(t, s, pathMinionControl+"/apps-install", adminHeader, url.Values{"worker_id": {"wB"}, "source_worker_id": {"wA"}, "if_state": {"all"}})
	if calls := f.called(ops.OpControlSetAccept); len(calls) != 2 || calls[1].Args["apps_install"] != false {
		t.Fatalf("calls = %+v", calls)
	}

	// A stored flag is ticked.
	fx.Workers[1].Accepts[0].AppsInstall = true
	f = newFakeOps().reply(ops.OpControlList, fx)
	connect(t, f)
	body = get(t, s, pathMinionControl, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `value="on" checked aria-label="Master sessions of boss-box may install apps on ci"`)

	// Limited: no box, and a click writes nothing.
	lim := acceptStatesFixture()
	lim.Workers[1].Accepts[0].AppsInstall = true
	f = newFakeOps().reply(ops.OpControlList, lim).reply(ops.OpControlSetAccept, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	body = get(t, s, pathMinionControl, "X-Test-Session", adminHeader).Body.String()
	mustNotHave(t, body, `aria-label="Master sessions of boss-box may install apps on ci"`)
	mustHave(t, body, `title="Master sessions of boss-box may install apps on ci">apps_install</span>`)
	rec = post(t, s, pathMinionControl+"/apps-install", adminHeader, url.Values{"worker_id": {"wB"}, "source_worker_id": {"wA"}, "value": {"on"}, "if_state": {"all"}})
	if calls := f.called(ops.OpControlSetAccept); len(calls) != 0 {
		t.Fatalf("a limited acceptance was written: %+v", calls)
	}
	mustHave(t, rec.Body.String(), "Set apps_install of a limited acceptance with the operation control_set_accept.")
}
