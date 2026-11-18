package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func shareListOf(t *testing.T, call func() (json.RawMessage, error)) ShareListResult {
	t.Helper()
	raw, err := call()
	if err != nil {
		t.Fatal(err)
	}
	var r ShareListResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// The list works on an unenrolled Solo Worker, under share_lock and in
// preview-only, and shows each session with its share state.
func TestShareListUnenrolledLockedPreviewSolo(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.host.addSession("private", "claude")
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"work"}}); err != nil {
		t.Fatal(err)
	}
	e.policy.SetShareLock(true)
	e.policy.SetPreviewOnly(true)
	s := shareLocal{st: e.st, ex: e.ex, policy: e.policy, overriddenBy: []string{}}
	r := shareListOf(t, func() (json.RawMessage, error) { return s.listOp(context.Background()) })
	if r.Rule != "select" || !r.Lock || r.Inventory != InventoryOK || len(r.Sessions) != 2 {
		t.Fatalf("list = %+v", r)
	}
	if r.Sessions[0] != (ShareListSession{Name: "private", Shared: false}) || r.Sessions[1] != (ShareListSession{Name: "work", Shared: true}) {
		t.Fatalf("sessions = %+v", r.Sessions)
	}
	// The owner may change it locally under share_lock and preview-only.
	r = shareListOf(t, func() (json.RawMessage, error) {
		return s.applyOp(context.Background(), mustJSON(t, LocalShareApplyArgs{Add: []string{"private"}, IfRevision: r.Revision}))
	})
	if !r.Sessions[0].Shared || !r.Sessions[1].Shared {
		t.Fatalf("after add = %+v", r)
	}
}

// A stale revision changes nothing and answers conflict/state_changed (the
// CLI's 27).
func TestShareApplyRevision(t *testing.T) {
	e := newEnv(t)
	s := shareLocal{st: e.st, ex: e.ex, policy: e.policy, overriddenBy: []string{}}
	r := shareListOf(t, func() (json.RawMessage, error) { return s.listOp(context.Background()) })
	_, err := s.applyOp(context.Background(), mustJSON(t, LocalShareApplyArgs{Mode: "none", IfRevision: r.Revision + 5}))
	wantAPIError(t, err, "conflict", "state_changed")
	if after := shareListOf(t, func() (json.RawMessage, error) { return s.listOp(context.Background()) }); after.Rule != r.Rule || after.Revision != r.Revision {
		t.Fatalf("changed: %+v -> %+v", r, after)
	}
}

// Listing errors are "unavailable", distinct from an empty host.
func TestInventoryUnavailableVsEmpty(t *testing.T) {
	e := newEnv(t)
	s := shareLocal{st: e.st, ex: e.ex, policy: e.policy, overriddenBy: []string{}}
	if r := shareListOf(t, func() (json.RawMessage, error) { return s.listOp(context.Background()) }); r.Inventory != InventoryOK || len(r.Sessions) != 0 {
		t.Fatalf("empty host = %+v", r)
	}
	e.ex.host = failingListHost{e.host}
	if r := shareListOf(t, func() (json.RawMessage, error) { return s.listOp(context.Background()) }); r.Inventory != InventoryUnavailable || len(r.Sessions) != 0 {
		t.Fatalf("failing host = %+v", r)
	}
}

func TestShareOverriddenBy(t *testing.T) {
	env := func(k string) (string, bool) { return "all", k == "MINION_SHARE_EXISTING" }
	s := Settings{Resolver: &config.Resolver{Role: config.ScopeWorker, LookupEnv: env, File: config.MapSource{config.KeyShareExisting: "none"}}}
	if got := shareOverriddenBy(s); len(got) != 1 || got[0] != "env" {
		t.Fatalf("env = %v", got)
	}
	s.Resolver = &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(string) (string, bool) { return "", false },
		File: config.MapSource{config.KeyShareExisting: "none"}}
	if got := shareOverriddenBy(s); len(got) != 0 {
		t.Fatalf("file only = %v", got)
	}
}
