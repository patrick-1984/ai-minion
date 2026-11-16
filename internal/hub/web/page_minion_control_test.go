package web

import (
	"net/http"
	"net/url"
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
		`<script src="/static/minion-control.js" defer></script>`)
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
