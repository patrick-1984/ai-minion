package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// The setting reaches the policy of the real core module, so the lock cannot
// be a form-only checkbox: the core's own executor refuses share_set under it,
// and the Hub's share_set cannot lift it. A start without the setting is
// unlocked, and a restart with the setting locks again whatever was saved.
func TestCoreModuleShareLockRefusesShareSet(t *testing.T) {
	dir := t.TempDir()
	locked := startCore(t, dir, config.WorkerCore{ShareExisting: config.ShareExistingNone, ShareLock: true})
	pol, _ := Lookup[*Policy](locked.rt)
	if !pol.ShareLocked() {
		t.Fatal("share_lock was not applied to the policy")
	}
	locked.host.addSession("work", "bash")
	ex, _ := Lookup[*Executor](locked.rt)
	st, _ := Lookup[*store.Store](locked.rt)
	// The Worker is enrolled under the id the share arguments name: a share
	// operation that names another Worker is refused before the lock is asked,
	// so without this the refusal below would not be the lock's. The core reads
	// the id from the stored enrollment at its start, and the connector sets it
	// from every welcome; the test sets it as they do.
	pol.SetWorkerID(shareWorkerID)
	_, err := ex.Execute(context.Background(), Request{Op: ops.OpShareSet, Actions: []ops.Action{ops.ActionOperate},
		Args: mustJSON(t, shareSetArgs("all"))})
	wantAPIError(t, err, "forbidden", "")
	if ae, _ := AsAPIError(err); ae == nil || !strings.Contains(ae.Message, "locked locally") {
		t.Fatalf("the refusal is not the lock's: %v", err)
	}
	if sel, _ := st.LoadShare(); sel.Mode != store.ShareNone {
		t.Fatalf("the locked Worker changed its selection: %+v", sel)
	}
	// The operator can still change it locally (the store is the local path).
	if err := st.SaveShare(store.ShareSelection{Mode: store.ShareAll}); err != nil {
		t.Fatal(err)
	}
	if err := locked.rt.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	unlocked := startCore(t, dir, config.WorkerCore{})
	pol2, _ := Lookup[*Policy](unlocked.rt)
	if pol2.ShareLocked() {
		t.Fatal("the lock outlived the setting")
	}
	pol2.SetWorkerID(shareWorkerID)
	ex2, _ := Lookup[*Executor](unlocked.rt)
	if _, err := ex2.Execute(context.Background(), Request{Op: ops.OpShareSet, Actions: []ops.Action{ops.ActionOperate},
		Args: mustJSON(t, shareSetArgs("none"))}); err != nil {
		t.Fatalf("an unlocked Worker refused share_set: %v", err)
	}
}
