package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestSessionControlModuleIsAfterTheAPIModule(t *testing.T) {
	names := DefaultRegistry.Names()
	idx := func(n string) int {
		for i, x := range names {
			if x == n {
				return i
			}
		}
		return -1
	}
	if idx("session_control") < 0 || idx("session_control") < idx("api") {
		t.Errorf("modules = %v", names)
	}
}

func TestSessionControlModuleDoesNothingForACatalogWithoutThoseOps(t *testing.T) {
	r := NewRegistry()
	registerSessionControlModule(r, ops.NewRegistry())
	if err := NewKernel(r).Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
}

func TestSessionControlModuleNamesMissingServices(t *testing.T) {
	r := NewRegistry()
	registerSessionControlModule(r, ops.Default)
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"session_control"`) {
		t.Errorf("err = %v", err)
	}
}

// On the real Hub the admin lists no grant on a fresh Hub, creates one, a plain
// user is refused, and removing the controller Worker deletes the grant.
func TestSessionControlOnTheRealHub(t *testing.T) {
	ah := startRemoveHub(t)
	ctx := context.Background()
	alice := ah.user(t, "alice")
	w := store.WorkerRow{ID: store.NewID(), Pub: mustKey(t).Public(), Name: "pi", OwnerID: alice,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}
	if err := ah.st.InsertWorker(ctx, nil, w); err != nil {
		t.Fatal(err)
	}
	// A second Worker for "everything" to reach (a Worker is never its own target).
	if err := ah.st.InsertWorker(ctx, nil, store.WorkerRow{ID: store.NewID(), Pub: mustKey(t).Public(), Name: "pi2", OwnerID: alice,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	op := func(user, name string, args map[string]any) apiResp {
		tok, csrf := ah.login(t, user)
		return ah.call(t, "POST", "/api/v1/ops/"+name, args, asSession(tok, csrf))
	}
	if r := op("root", ops.OpSessionControlList, map[string]any{}); r.code != 200 || !strings.Contains(r.body, `"grants":[]`) {
		t.Fatalf("list on a fresh Hub = %d %s; want no grants", r.code, r.body)
	}
	set := map[string]any{"request_id": ops.NewUUIDv7(), "worker_id": w.ID, "session_name": "boss",
		"targets": []ops.KeyResource{{Kind: ops.KeyResourceEverything}}}
	if r := op("alice", ops.OpSessionControlSet, set); r.code != 403 {
		t.Fatalf("set by the Worker's owner = %d %s, want 403", r.code, r.body)
	}
	if r := op("root", ops.OpSessionControlSet, set); r.code != 200 {
		t.Fatalf("set by the admin = %d %s", r.code, r.body)
	}
	r := op("root", ops.OpSessionControlList, map[string]any{})
	var res ops.SessionControlListResult
	if err := json.Unmarshal([]byte(r.body), &res); err != nil || len(res.Grants) != 1 || res.Grants[0].WorkerID != w.ID {
		t.Fatalf("list = %s, %v", r.body, err)
	}
	if r := op("alice", ops.OpWorkerRemove, map[string]any{"request_id": ops.NewUUIDv7(), "worker_id": w.ID}); r.code != 200 {
		t.Fatalf("worker_remove = %d %s", r.code, r.body)
	}
	if rows, err := ah.st.ListSessionControl(ctx); err != nil || len(rows) != 0 {
		t.Errorf("grants after the removal = %+v, %v", rows, err)
	}
}
