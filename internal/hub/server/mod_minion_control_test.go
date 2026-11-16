package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// On the real Hub: an admin nominates a master session and lets another Worker
// accept its Worker; the computed grant appears; a plain user is refused the
// control ops; a Worker that is not user-managed refuses people its sessions.
func TestMinionControlOnTheRealHub(t *testing.T) {
	ah := startRemoveHub(t)
	ctx := context.Background()
	alice := ah.user(t, "alice")
	mk := func(name string) store.WorkerRow {
		w := store.WorkerRow{ID: store.NewID(), Pub: mustKey(t).Public(), Name: name, OwnerID: alice,
			Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}
		if err := ah.st.InsertWorker(ctx, nil, w); err != nil {
			t.Fatal(err)
		}
		return w
	}
	boss, target := mk("boss-box"), mk("ci")
	op := func(user, name string, args map[string]any) apiResp {
		if name != ops.OpControlList && name != ops.OpControlCheck {
			args["request_id"] = ops.NewUUIDv7()
		}
		tok, csrf := ah.login(t, user)
		return ah.call(t, "POST", "/api/v1/ops/"+name, args, asSession(tok, csrf))
	}
	if r := op("alice", ops.OpControlList, map[string]any{}); r.code != 403 {
		t.Fatalf("control_list by a user = %d %s, want 403", r.code, r.body)
	}
	if r := op("root", ops.OpControlSetMaster, map[string]any{"worker_id": boss.ID, "session_name": "boss", "master": true}); r.code != 200 {
		t.Fatalf("set master = %d %s", r.code, r.body)
	}
	if rows, _ := ah.st.ListSessionControl(ctx); len(rows) != 0 {
		t.Fatalf("a master without acceptance reached something: %+v", rows)
	}
	if r := op("root", ops.OpControlSetAccept, map[string]any{"worker_id": target.ID, "source_worker_id": boss.ID, "accept": true}); r.code != 200 {
		t.Fatalf("set accept = %d %s", r.code, r.body)
	}
	rows, _ := ah.st.ListSessionControl(ctx)
	if len(rows) != 1 || rows[0].WorkerID != boss.ID || !strings.Contains(rows[0].TargetsJSON, target.ID) {
		t.Fatalf("computed grant = %+v", rows)
	}
	r := op("root", ops.OpControlCheck, map[string]any{"worker_id": target.ID, "session_name": "build"})
	var chk ops.ControlCheckResult
	if err := json.Unmarshal([]byte(r.body), &chk); err != nil || len(chk.Controllers) != 1 || chk.Controllers[0].SessionName != "boss" {
		t.Fatalf("check = %s", r.body)
	}
	// Not user-managed: people are refused the Worker's sessions (before routing).
	if r := op("root", ops.OpControlSetWorker, map[string]any{"worker_id": target.ID, "user_managed": false}); r.code != 200 {
		t.Fatalf("set worker = %d %s", r.code, r.body)
	}
	tok, csrf := ah.login(t, "alice")
	r = ah.call(t, "GET", "/api/v1/ops/sessions_list?worker_id="+target.ID, nil, asSession(tok, csrf))
	var env struct {
		Error struct {
			Code proto.Code `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(r.body), &env)
	if env.Error.Code != proto.CodeForbidden {
		t.Fatalf("sessions_list on a Worker that is not user-managed = %d %s", r.code, r.body)
	}
	// The other Worker stays user-managed (it is offline: routed, not refused).
	r = ah.call(t, "GET", "/api/v1/ops/sessions_list?worker_id="+boss.ID, nil, asSession(tok, csrf))
	_ = json.Unmarshal([]byte(r.body), &env)
	if env.Error.Code != proto.CodeWorkerOffline {
		t.Fatalf("sessions_list on a user-managed Worker = %d %s", r.code, r.body)
	}
}

func TestOnlyFullWorkersCanBeMasters(t *testing.T) {
	full := workers.Online{Facts: transport.WorkerFacts{Build: "v0.9.0"}}
	old := workers.Online{Features: []string{proto.FeaturePipeCausal, proto.FeatureTextVerbatim}}
	thin := workers.Online{Features: []string{proto.FeatureTextVerbatim}}
	if !canBeMaster(full, true) || !canBeMaster(old, true) || canBeMaster(thin, true) || !canBeMaster(thin, false) {
		t.Fatal("full and 0.8 online = master, thin online = not, offline = may be")
	}
}
