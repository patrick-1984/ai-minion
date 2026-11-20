package cli

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// ---- worker config share list / --revision (picker) ----

type listerNames []string

func (l listerNames) ListSessions(context.Context) ([]term.Session, error) {
	var out []term.Session
	for _, n := range l {
		out = append(out, term.Session{Name: n})
	}
	return out, nil
}

func shareList(t *testing.T, cd workerConfigDeps, dir string) shareListReply {
	t.Helper()
	out, _, err := configCmd(t, cd, dir, "share", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var r shareListReply
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return r
}

// The listing is names only, shows every session of the host (also the ones
// the rule hides) with its checkbox, keeps saved names of sessions that are
// gone, and reports share_lock.
func TestShareListIsNamesOnlyAndShowsHiddenSessions(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(map[string]string{"MINION_SHARE_LOCK": "true"})
	cd.sessions = func(string) agent.StatusSessions { return listerNames{"zeta", "alpha", "beta"} }
	if _, _, err := configCmd(t, cd, dir, "share", "select", "beta", "gone"); err != nil {
		t.Fatal(err)
	}
	r := shareList(t, cd, dir)
	want := []shareListSession{{"alpha", false}, {"beta", true}, {"zeta", false}}
	if r.Mode != "select" || !reflect.DeepEqual(r.Sessions, want) || !reflect.DeepEqual(r.Selected, []string{"beta", "gone"}) ||
		!r.Locked || !r.SessionsKnown || r.Revision == "" {
		t.Fatalf("list = %+v", r)
	}
	out, _, err := configCmd(t, cd, dir, "share", "list")
	if err != nil || !strings.Contains(out, "[x] beta") || !strings.Contains(out, "[ ] alpha") || !strings.Contains(out, "share_lock is on") {
		t.Fatalf("text list: %q %v", out, err)
	}
	// Without a listing (Windows) the saved selection is all there is.
	cd.sessions = func(string) agent.StatusSessions { return nil }
	if r := shareList(t, cd, dir); r.SessionsKnown || len(r.Sessions) != 0 || len(r.Selected) != 2 {
		t.Fatalf("no listing: %+v", r)
	}
}

// A checked edit carries the revision of the listing: when the choice changed
// since, nothing is changed and the command fails; with the current revision it
// is saved.
func TestShareCheckedEditRefusesOnConflict(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(nil)
	cd.sessions = func(string) agent.StatusSessions { return listerNames{"a", "b"} }
	if _, _, err := configCmd(t, cd, dir, "share", "select", "a", "b"); err != nil {
		t.Fatal(err)
	}
	rev := shareList(t, cd, dir).Revision
	// Another operator hides b meanwhile.
	if _, _, err := configCmd(t, cd, dir, "share", "select", "a"); err != nil {
		t.Fatal(err)
	}
	_, _, err := configCmd(t, cd, dir, "share", "select", "a", "b", "--revision", rev)
	if err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("a stale edit was accepted: %v", err)
	}
	if sel := loadShare(t, dir); !reflect.DeepEqual(sel.Sessions, []string{"a"}) {
		t.Fatalf("the stale edit changed the choice: %+v", sel)
	}
	cur := shareList(t, cd, dir).Revision
	if _, _, err := configCmd(t, cd, dir, "share", "select", "b", "--revision", cur); err != nil {
		t.Fatal(err)
	}
	if sel := loadShare(t, dir); !reflect.DeepEqual(sel.Sessions, []string{"b"}) {
		t.Fatalf("share = %+v", sel)
	}
}

// A change of the mode keeps the names that are saved when it commits; select
// without names keeps them too, and refuses when there are none.
func TestShareModeOnlyKeepsNames(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(nil)
	if _, _, err := configCmd(t, cd, dir, "share", "select", "a", "b"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"none", "all", "later"} {
		if _, _, err := configCmd(t, cd, dir, "share", mode); err != nil {
			t.Fatal(err)
		}
		if sel := loadShare(t, dir); !reflect.DeepEqual(sel.Sessions, []string{"a", "b"}) {
			t.Fatalf("%s dropped the names: %+v", mode, sel)
		}
	}
	if _, _, err := configCmd(t, cd, dir, "share", "select"); err != nil {
		t.Fatal(err)
	}
	if sel := loadShare(t, dir); sel.Mode != store.ShareSelect || len(sel.Sessions) != 2 {
		t.Fatalf("share = %+v", sel)
	}
	empty := t.TempDir()
	if _, _, err := configCmd(t, cd, empty, "share", "select"); err == nil {
		t.Fatal("select with no names and none saved was accepted")
	}
}

// The mode-only change reads the names in the write transaction: a name that
// another writer saved between a caller's read and its write is not dropped,
// and a stale revision changes nothing (store level).
func TestStoreCommitShareChangeIsOneTransaction(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	first, err := st.CommitShareChange(ctx, store.ShareChange{Mode: store.ShareSelect, Names: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	// Someone else adds b after our reader looked.
	if _, err := st.CommitShareChange(ctx, store.ShareChange{Mode: store.ShareSelect, Names: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.CommitShareChange(ctx, store.ShareChange{Mode: store.ShareNone})
	if err != nil || !reflect.DeepEqual(got.Sessions, []string{"a", "b"}) {
		t.Fatalf("mode-only change: %+v %v", got, err)
	}
	_, err = st.CommitShareChange(ctx, store.ShareChange{Mode: store.ShareAll, ExpectRevision: first.Revision})
	if !errors.Is(err, store.ErrShareConflict) {
		t.Fatalf("a stale revision: %v", err)
	}
	if cur, _ := st.LoadShareStateContext(ctx); cur.Mode != store.ShareNone {
		t.Fatalf("the refused change was applied: %+v", cur)
	}
}

// The commands touch only the state database, which a running Worker (of any
// version, v0.14.0 included) has open: its own handle sees each change at
// once, and nothing needs a new operation of the Worker.
func TestShareCommandsWorkWhileAWorkerHoldsTheState(t *testing.T) {
	dir := t.TempDir()
	worker, err := store.Open(dir) // what a running Worker holds
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = worker.Close() }()
	cd := testConfigDeps(nil)
	cd.sessions = func(string) agent.StatusSessions { return listerNames{"a", "b"} }
	rev := shareList(t, cd, dir).Revision
	if _, _, err := configCmd(t, cd, dir, "share", "select", "a", "--revision", rev); err != nil && rev != "" {
		t.Fatal(err)
	}
	if _, _, err := configCmd(t, cd, dir, "share", "select", "a"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := worker.IsShared("a"); !ok {
		t.Fatal("the Worker does not see the shared session")
	}
	if ok, _ := worker.IsShared("b"); ok {
		t.Fatal("the Worker shares an unchecked session")
	}
}
