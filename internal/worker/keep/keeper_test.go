package keep

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// fakeTerm is a terminal host whose sessions have a layout; only what the
// keeper uses is implemented.
type fakeTerm struct {
	term.SessionHost
	mu       sync.Mutex
	sessions map[string]term.SessionLayout
	rebuilt  []string
	// rebuild, when set, is what RebuildSession does instead of creating the
	// session with its layout.
	rebuild func(name string, l term.SessionLayout) error
}

func (f *fakeTerm) ListSessions(context.Context) ([]term.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []term.Session{}
	for n := range f.sessions {
		out = append(out, term.Session{Name: n})
	}
	return out, nil
}

func (f *fakeTerm) SessionLayout(_ context.Context, name string) (term.SessionLayout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.sessions[name]
	if !ok {
		return term.SessionLayout{}, term.ErrNotFound
	}
	return l, nil
}

func (f *fakeTerm) RebuildSession(_ context.Context, name string, l term.SessionLayout, _ string, _ func(term.PaneLayout, string) []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebuilt = append(f.rebuilt, name)
	if _, ok := f.sessions[name]; ok {
		return term.ErrSessionExists
	}
	if f.rebuild != nil {
		return f.rebuild(name, l)
	}
	f.sessions[name] = l
	return nil
}

func (f *fakeTerm) ServerID(context.Context) (string, error) { return "1 1", nil }

func twoPanes(cwd string) term.SessionLayout {
	return term.SessionLayout{Windows: []term.WindowLayout{{Name: "main", Panes: []term.PaneLayout{{Cwd: cwd}, {Index: 1, Cwd: cwd}}}}}
}

// keptStore returns a store whose keep list has the names with a saved
// two-pane layout.
func keptStore(t *testing.T, names ...string) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, n := range names {
		b, _ := json.Marshal(twoPanes("/srv/" + n))
		if err := st.KeepSession(ctx, n, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SaveKeptLayout(ctx, n, "", string(b)); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func savedLayout(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	rows, err := st.KeptSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r.Layout
		}
	}
	t.Fatalf("%s is not kept", name)
	return ""
}

// A kept session the share rule does not share now is not restored and
// keeps its row; a shared one is.
func TestRestoreSkipsKeptSessionsThatAreNotShared(t *testing.T) {
	ctx := context.Background()
	st := keptStore(t, "private", "shared")
	h := &fakeTerm{sessions: map[string]term.SessionLayout{}}
	k, err := New(Config{Host: h, Layouts: h, Store: st, Shared: func(_ context.Context, n string) (bool, error) { return n == "shared", nil }})
	if err != nil {
		t.Fatal(err)
	}
	done, err := k.Restore(ctx)
	if err != nil || !slices.Equal(done, []string{"shared"}) || slices.Contains(h.rebuilt, "private") {
		t.Fatalf("restored %v (rebuilt %v), %v", done, h.rebuilt, err)
	}
	if names, _ := st.KeptNames(ctx); !names["private"] {
		t.Fatal("the row of an unshared kept session was dropped")
	}
}

// On a preview-only Worker the restore pass recreates nothing, keeps the
// rows and stays due: it runs once the Worker is writable again.
func TestRestoreDoesNothingOnAPreviewOnlyWorker(t *testing.T) {
	ctx := context.Background()
	st := keptStore(t, "work")
	h := &fakeTerm{sessions: map[string]term.SessionLayout{}}
	preview := true
	k, err := New(Config{Host: h, Layouts: h, Store: st, PreviewOnly: func() bool { return preview }})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if !k.Tick(ctx) {
			t.Fatal("a preview-only pass waits as for a host that is down")
		}
	}
	if len(h.rebuilt) != 0 {
		t.Fatalf("rebuilt on a preview-only Worker: %v", h.rebuilt)
	}
	if names, _ := st.KeptNames(ctx); !names["work"] {
		t.Fatal("the row was dropped")
	}
	preview = false
	k.Tick(ctx)
	if !slices.Equal(h.rebuilt, []string{"work"}) {
		t.Fatalf("after preview-only ended, rebuilt %v", h.rebuilt)
	}
}

// A rebuild that fails part way leaves a session with fewer panes; the save
// pass of the same Tick does not put that over the good saved layout.
func TestFailedRestoreDoesNotOverwriteTheSavedLayout(t *testing.T) {
	ctx := context.Background()
	st := keptStore(t, "work")
	good := savedLayout(t, st, "work")
	h := &fakeTerm{sessions: map[string]term.SessionLayout{}}
	h.rebuild = func(name string, l term.SessionLayout) error {
		part := l
		part.Windows = []term.WindowLayout{{Name: "main", Panes: l.Windows[0].Panes[:1]}}
		h.sessions[name] = part
		return errors.New("tmux: split-window failed")
	}
	k, err := New(Config{Host: h, Layouts: h, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	k.Tick(ctx)
	if len(h.rebuilt) != 1 {
		t.Fatalf("rebuilt %v", h.rebuilt)
	}
	if got := savedLayout(t, st, "work"); got != good {
		t.Fatalf("the saved layout was overwritten:\n got %s\nwant %s", got, good)
	}
	audit, err := st.ListAudit(ctx, 10)
	if err != nil || len(audit) != 1 || audit[0].Reason != "restore_failed" {
		t.Fatalf("audit = %+v, %v", audit, err)
	}
}

// A name that is taken when the rebuild runs is skipped, not audited as a
// failure.
func TestRestoreSkipsANameTakenMeanwhile(t *testing.T) {
	ctx := context.Background()
	st := keptStore(t, "work")
	h := &fakeTerm{sessions: map[string]term.SessionLayout{}}
	h.rebuild = func(string, term.SessionLayout) error { return term.ErrSessionExists }
	k, err := New(Config{Host: h, Layouts: h, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	if done, err := k.Restore(ctx); err != nil || len(done) != 0 {
		t.Fatalf("restore = %v, %v", done, err)
	}
	if audit, _ := st.ListAudit(ctx, 10); len(audit) != 0 {
		t.Fatalf("a taken name was audited: %+v", audit)
	}
}
