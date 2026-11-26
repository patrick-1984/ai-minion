package agent

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type fakeKeep struct {
	mu    sync.Mutex
	names map[string]string // name -> instance id
}

func (f *fakeKeep) Keep(_ context.Context, name, instance string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names[name] = instance
	return nil
}

func (f *fakeKeep) Unkeep(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.names, name)
	return nil
}

func (f *fakeKeep) Kept(context.Context) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for n := range f.names {
		out[n] = true
	}
	return out, nil
}

func keepArgs(session string, keep bool) ops.SessionKeepSetArgs {
	return ops.SessionKeepSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target(session), Keep: keep}
}

func TestSessionKeepSetIsAnnouncedOnlyWithAKeepList(t *testing.T) {
	e := newEnv(t)
	if slices.Contains(e.ex.Ops(), ops.OpSessionKeepSet) || slices.Contains(e.ex.Features(), proto.FeatureSessionKeepV1) {
		t.Fatal("announced without a keep list")
	}
	_, err := e.write(ops.OpSessionKeepSet, keepArgs("work", true))
	wantAPIError(t, err, "unsupported_op", "")
	e.ex.AttachKeep(&fakeKeep{names: map[string]string{}})
	if !slices.Contains(e.ex.Ops(), ops.OpSessionKeepSet) || !slices.Contains(e.ex.Features(), proto.FeatureSessionKeepV1) {
		t.Fatalf("not announced: ops %v features %v", e.ex.Ops(), e.ex.Features())
	}
}

func TestSessionKeepSetKeepsListsAndForgets(t *testing.T) {
	e := newEnv(t)
	k := &fakeKeep{names: map[string]string{}}
	e.ex.AttachKeep(k)
	e.host.addSession("work", "bash")
	e.host.addSession("other", "bash")

	if res := e.mustWrite(ops.OpSessionKeepSet, keepArgs("work", true)); res.Outcome != ops.OutcomeDelivered || res.Pane == nil || res.Pane.Session != "work" {
		t.Fatalf("keep = %+v", res)
	}
	if _, ok := k.names["work"]; !ok {
		t.Fatal("work was not kept")
	}
	raw, err := e.call(ops.OpSessionsList, allActions, ops.SessionsListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	var list ops.SessionsListResult
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	for _, s := range list.Sessions {
		if s.Keep != (s.Name == "work") {
			t.Fatalf("listing %s keep=%v", s.Name, s.Keep)
		}
	}

	// keep=true needs the session; keep=false also forgets a session that is gone.
	_, err = e.write(ops.OpSessionKeepSet, keepArgs("ghost", true))
	wantAPIError(t, err, "target_not_found", "")
	k.names["ghost"] = ""
	if res := e.mustWrite(ops.OpSessionKeepSet, keepArgs("ghost", false)); res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("unkeep of a gone session = %+v", res)
	}
	if _, ok := k.names["ghost"]; ok {
		t.Fatal("ghost is still kept")
	}
	// Reading is not enough, and every call leaves an audit row.
	_, err = e.writeAs(ops.OpSessionKeepSet, []ops.Action{ops.ActionRead}, keepArgs("work", false))
	wantAPIError(t, err, "forbidden", "")
	n := 0
	for _, a := range e.audit() {
		if a.Op == ops.OpSessionKeepSet {
			n++
		}
	}
	if n < 3 {
		t.Fatalf("%d audit rows of session_keep_set", n)
	}
}
