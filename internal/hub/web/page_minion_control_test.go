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
