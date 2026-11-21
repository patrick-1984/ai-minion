package web

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func boundControlFixture() ops.ControlListResult {
	f := controlFixture()
	source := &f.Workers[0]
	source.MasterSessionV1 = true
	source.MasterCandidates = []ops.ControlMasterCandidate{
		{MasterCandidate: proto.MasterCandidate{SessionName: "integration build and fixture validation", IncarnationID: "binding-new", Adapter: proto.MasterAdapterClaude, CatalogResult: proto.MasterCatalogEmpty}, Attachment: "ready"},
	}
	f.Workers[1].BoundMasters = []ops.ControlBoundMaster{
		{SessionName: "offline documentation preview", IncarnationID: "binding-old", Generation: 11, Master: true, Attachment: "offline"},
	}
	f.Workers[1].Online = false
	return f
}

func TestMinionControlBoundFormsUseObservedIdentityAndRevision(t *testing.T) {
	f := newFakeOps().reply(ops.OpControlList, boundControlFixture()).
		reply(ops.OpControlSetBoundMaster, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	server := adminServer(t)
	if r := post(t, server, pathMinionControl+"/bound-master", userHeader, url.Values{}); r.Code != http.StatusNotFound {
		t.Fatalf("non-admin bound mutation returned %d", r.Code)
	}
	body := get(t, server, pathMinionControl, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, "Nominate this attachment", "Last catalog served: empty", "Name-only legacy masters", "Revoke master access",
		`name="incarnation_id" value="binding-new"`, `name="if_generation" value="0"`,
		`name="incarnation_id" value="binding-old"`, `name="if_generation" value="11"`)
	res := post(t, server, pathMinionControl+"/bound-master", adminHeader, url.Values{
		"worker_id": {"wB"}, "session_name": {"offline documentation preview"}, "incarnation_id": {"binding-old"},
		"if_generation": {"11"}, "value": {"off"},
	})
	mustHave(t, res.Body.String(), "Revoked master access for offline documentation preview.")
	calls := f.called(ops.OpControlSetBoundMaster)
	if len(calls) != 1 || calls[0].Args["master"] != false || calls[0].Args["if_generation"] != float64(11) ||
		calls[0].Args["incarnation_id"] != "binding-old" || len(f.called(ops.OpControlSetMaster)) != 0 {
		t.Fatalf("revocation lost its observed binding or fell back: %+v", calls)
	}
}
