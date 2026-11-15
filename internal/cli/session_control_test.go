package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/client"
)

// Session control (SC-5): without MINION_HUB_URL, workers_list and
// sessions_list --hub-worker go to the local Worker, which forwards them to its
// Hub. With a Hub URL nothing changes.

func TestWorkersListWithoutAHubGoesToTheLocalWorker(t *testing.T) {
	local := localAnswers(`{"workers":[]}`)
	code, _, errs := runWith(t, depsWithSettings(local, map[string]string{}), "", "workers_list")
	if code != 0 || local.count() != 1 || local.last(t).Op != "workers_list" {
		t.Fatalf("exit %d (%s), local calls %d", code, errs, local.count())
	}
}

func TestWorkersListWithAHubURLStillGoesToTheHub(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, _ *http.Request, _ *hubStub) {
		stubJSON(w, 200, `{"workers":[]}`)
	})
	local := localAnswers(`{"workers":[]}`)
	code, _, errs := runWith(t, depsWithSettings(local, hub.env()), "", "workers_list")
	if code != 0 || hub.hits.Load() != 1 || local.count() != 0 {
		t.Fatalf("exit %d (%s), hub hits %d, local calls %d", code, errs, hub.hits.Load(), local.count())
	}
}

func TestSessionsListWithHubWorkerAndNoHubGoesToTheLocalWorkerWithTheWorkerID(t *testing.T) {
	local := localAnswers(`{"sessions":[]}`)
	code, _, errs := runWith(t, depsWithSettings(local, map[string]string{}), "", "sessions_list", "--hub-worker", hubWorkerID)
	if code != 0 || local.count() != 1 {
		t.Fatalf("exit %d (%s), local calls %d", code, errs, local.count())
	}
	var a struct {
		WorkerID string `json:"worker_id"`
	}
	if err := json.Unmarshal(local.last(t).Args, &a); err != nil || a.WorkerID != hubWorkerID {
		t.Fatalf("the local Worker got %s, want worker_id %s", local.last(t).Args, hubWorkerID)
	}

	// The same from the environment.
	local = localAnswers(`{"sessions":[]}`)
	if code, _, errs := runWith(t, depsWithSettings(local, map[string]string{"MINION_HUB_WORKER": hubWorkerID}), "", "sessions_list"); code != 0 || !strings.Contains(string(local.last(t).Args), hubWorkerID) {
		t.Fatalf("env: exit %d (%s), args %s", code, errs, local.last(t).Args)
	}

	// Without --hub-worker the listing stays the local Worker's own.
	local = localAnswers(`{"sessions":[]}`)
	if code, _, errs := runWith(t, depsWithSettings(local, map[string]string{}), "", "sessions_list"); code != 0 || strings.Contains(string(local.last(t).Args), "worker_id") {
		t.Fatalf("plain: exit %d (%s), args %s", code, errs, local.last(t).Args)
	}
}

func TestSessionsListWithAWorkerNameAndNoHubIsRefused(t *testing.T) {
	local := localAnswers(`{"sessions":[]}`)
	code, _, errs := runWith(t, depsWithSettings(local, map[string]string{}), "", "sessions_list", "--hub-worker", "build-box")
	if code != ExitUsage || local.count() != 0 || !strings.Contains(errs, "workers_list") {
		t.Fatalf("exit %d, stderr %q, local calls %d", code, errs, local.count())
	}
}

func TestSameWorkerTargetStaysAsGiven(t *testing.T) {
	// A target without a Worker goes to the local Worker exactly as before:
	// no Worker part is added.
	local := &fakeOpsBackend{call: answerWith(json.RawMessage(`{"screen":{}}`), nil)}
	if code, _, errs := runWith(t, depsWithSettings(local, map[string]string{"MINION_HUB_WORKER": hubWorkerID}), "", "session_snapshot", "--target", "work"); code != 0 || local.count() != 1 {
		t.Fatalf("exit %d (%s), local calls %d", code, errs, local.count())
	}
	var a struct {
		Target struct {
			WorkerID string `json:"worker_id"`
		} `json:"target"`
	}
	if err := json.Unmarshal(local.last(t).Args, &a); err != nil || a.Target.WorkerID != "" {
		t.Fatalf("args %s name a Worker", local.last(t).Args)
	}
}

func TestMCPWithoutAHubNamesTheHubWorkerInSessionsList(t *testing.T) {
	local := localAnswers(`{"sessions":[]}`)
	deps := mcpDeps{
		lookupEnv: envFrom(map[string]string{"MINION_HUB_WORKER": hubWorkerID, "MINION_STATE_DIR": ""}),
		local:     func() client.Backend { return local },
	}
	b, _, err := selectMCPBackend(newMCPCmd(deps), deps, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call(context.Background(), "sessions_list", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(local.last(t).Args), hubWorkerID) {
		t.Fatalf("args %s, want worker_id %s", local.last(t).Args, hubWorkerID)
	}
}

// TestMCPWorkersListIsAToolOnTheLocalWorker: without a Hub, minion mcp lists
// workers_list and calls it on the local Worker, which forwards it (SC-5).
func TestMCPWorkersListIsAToolOnTheLocalWorker(t *testing.T) {
	local := &stubBackend{out: map[string]string{"workers_list": `{"workers":[{"id":"w2","name":"two"}]}`}}
	deps, _ := hubMCPDeps(map[string]string{}, local)
	got, _ := rpc(t, deps, nil, listRequest,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"workers_list","arguments":{}}}`)
	if !toolNamesOf(t, got[2])["workers_list"] {
		t.Fatalf("tools/list = %s", got[2])
	}
	if !strings.Contains(string(got[3]), "two") || strings.Contains(string(got[3]), `"isError":true`) {
		t.Fatalf("tools/call = %s", got[3])
	}
	if ops := local.ops(); len(ops) != 1 || ops[0] != "workers_list" {
		t.Fatalf("local ops = %v", ops)
	}
}
