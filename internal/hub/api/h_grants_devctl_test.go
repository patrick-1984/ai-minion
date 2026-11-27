package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// grant_set asks the target guard and stores nothing when it refuses: a
// Worker with device control on stays unshared.
func TestGrantSetRefusedByTheDeviceControlGuard(t *testing.T) {
	k := newKindStack(t)
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpGrantList, ops.OpGrantSet, ops.OpGrantRevoke, ops.OpGrantApprove} {
		op, _ := ops.Lookup(n)
		cat.Register(op)
	}
	handlers := NewRegistry(cat)
	list := func(ctx context.Context) ([]GrantWorker, error) {
		rows, err := k.st.ListWorkers(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]GrantWorker, 0, len(rows))
		for _, w := range rows {
			out = append(out, GrantWorker{ID: w.ID, Name: w.Name, OwnerID: w.OwnerID, Approved: true, CanBeSource: true})
		}
		return out, nil
	}
	var asked []string
	var on map[string]bool
	guard := func(_ context.Context, _ authz.Principal, id string) error {
		asked = append(asked, id)
		if on[id] {
			return NewError(proto.CodeConflict, "device control (on-device adb) is on for this worker")
		}
		return nil
	}
	lg := audit.New(k.st)
	if err := RegisterGrants(handlers, k.st, list, k.az, lg, WithGrantTargetGuard(guard)); err != nil {
		t.Fatal(err)
	}
	k.mux = http.NewServeMux()
	d := &Dispatcher{Catalog: cat, Handlers: handlers, Auth: k.authn, Authz: k.az, Audit: lg, Log: testLogger(&strings.Builder{})}
	if _, err := NewRouter(d, k.mux); err != nil {
		t.Fatal(err)
	}
	a1, tv := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "tv", store.WorkerKindPrivate)
	on = map[string]bool{tv.ID: true}
	k.as(k.u1)
	code, body := k.call(ops.OpGrantSet, grantArgs(a1, tv, ops.GrantLevelView))
	if kindErrCode(t, body) != proto.CodeConflict || !strings.Contains(body, "device control") {
		t.Fatalf("grant to the TV = %d %s", code, body)
	}
	if len(asked) != 1 || asked[0] != tv.ID {
		t.Fatalf("asked %v", asked)
	}
	if l := k.listGrants(); len(l) != 0 {
		t.Fatalf("a refused grant was stored: %+v", l)
	}
	// The other direction (the TV's session reaching another Worker) does not
	// share the TV.
	k.setGrant(grantArgs(tv, a1, ops.GrantLevelView))
}
