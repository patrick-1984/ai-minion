package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestApplyShareChange(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	st, err := s.ApplyShareChangeContext(ctx, ShareChange{Mode: ShareSelect, Add: []string{"a", "b"}})
	if err != nil || st.Mode != ShareSelect || !slices.Equal(st.Sessions, []string{"a", "b"}) {
		t.Fatalf("select a b = %+v, %v", st, err)
	}
	st, err = s.ApplyShareChangeContext(ctx, ShareChange{Add: []string{"c"}, Remove: []string{"a"}, IfRevision: st.Revision})
	if err != nil || !slices.Equal(st.Sessions, []string{"b", "c"}) {
		t.Fatalf("add c remove a = %+v, %v", st, err)
	}
	if _, err := s.ApplyShareChangeContext(ctx, ShareChange{Add: []string{"d"}, IfRevision: st.Revision - 1}); !errors.Is(err, ErrShareRevision) {
		t.Fatalf("stale revision = %v", err)
	}
	// Removing the last ones leaves select with nothing shared.
	st, err = s.ApplyShareChangeContext(ctx, ShareChange{Remove: []string{"b", "c"}})
	if err != nil || st.Mode != ShareSelect || len(st.Sessions) != 0 {
		t.Fatalf("empty select = %+v, %v", st, err)
	}
	if shared, _ := s.IsShared("b"); shared {
		t.Fatal("an empty select shares a session")
	}
	if _, err := s.ApplyShareChangeContext(ctx, ShareChange{Mode: ShareAll, Add: []string{"x"}}); !errors.Is(err, ErrInvalidShare) {
		t.Fatalf("add under all = %v", err)
	}
	st, err = s.ApplyShareChangeContext(ctx, ShareChange{Mode: ShareAll})
	if err != nil || st.Mode != ShareAll {
		t.Fatalf("all = %+v, %v", st, err)
	}
	// A new select starts from the names given, not an old list.
	st, err = s.ApplyShareChangeContext(ctx, ShareChange{Mode: ShareSelect, Add: []string{"z"}})
	if err != nil || !slices.Equal(st.Sessions, []string{"z"}) {
		t.Fatalf("new select = %+v, %v", st, err)
	}
}

// Gate 1: Replace makes the selection exactly the names given, whatever the
// saved selection is when the transaction reads it (a Hub edit that happened
// after the caller listed it), and an empty replacement shares nothing.
func TestApplyShareChangeReplaceIsExact(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if _, err := s.ApplyShareChangeContext(ctx, ShareChange{Mode: ShareSelect, Add: []string{"old1", "old2"}}); err != nil {
		t.Fatal(err)
	}
	// The Hub made it "private" meanwhile; the CLI's computed removals would
	// have named old1 and old2 only.
	if _, err := s.ApplyShareChangeContext(ctx, ShareChange{Add: []string{"private"}, Remove: []string{"old1", "old2"}}); err != nil {
		t.Fatal(err)
	}
	st, err := s.ApplyShareChangeContext(ctx, ShareChange{Mode: ShareSelect, Add: []string{"work"}, Replace: true})
	if err != nil || !slices.Equal(st.Sessions, []string{"work"}) {
		t.Fatalf("replace by work = %+v, %v", st, err)
	}
	if shared, _ := s.IsShared("private"); shared {
		t.Fatal("a session the replacement did not name is still shared")
	}
	st, err = s.ApplyShareChangeContext(ctx, ShareChange{Mode: ShareSelect, Replace: true})
	if err != nil || st.Mode != ShareSelect || len(st.Sessions) != 0 {
		t.Fatalf("empty replace = %+v, %v", st, err)
	}
	if _, err := s.ApplyShareChangeContext(ctx, ShareChange{Mode: ShareAll, Replace: true}); !errors.Is(err, ErrInvalidShare) {
		t.Fatalf("replace under all = %v", err)
	}
}

// Gate 1: an iteration error while the saved selection is read aborts the
// change before any write: the selection and its revision stay.
func TestApplyShareChangeIterationErrorChangesNothing(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	before, err := s.ApplyShareChangeContext(context.Background(), ShareChange{Mode: ShareSelect, Add: []string{"a", "b", "c"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	testHookAfterSelectedRow = cancel // the read fails after its first row
	t.Cleanup(func() { testHookAfterSelectedRow = nil })
	if _, err := s.ApplyShareChangeContext(ctx, ShareChange{Add: []string{"d"}, Remove: []string{"a"}}); err == nil {
		t.Fatal("a failed read of the selection was applied")
	}
	testHookAfterSelectedRow = nil
	after, err := s.LoadShareStateContext(context.Background())
	if err != nil || after.Revision != before.Revision || !slices.Equal(after.Sessions, before.Sessions) {
		t.Fatalf("after the failed change: %+v, %v; want %+v", after, err, before)
	}
}

// Gate 1: a selection that the local API could not list is refused before it
// is committed, and a replacement (set none) always works.
func TestApplyShareChangeSelectionSizeBudget(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	chunk := func(prefix string, n int) []string {
		var out []string
		for i := range n {
			out = append(out, fmt.Sprintf("%s-%04d-%s", prefix, i, strings.Repeat("x", 900)))
		}
		return out
	}
	var last ShareState
	for i := 0; ; i++ {
		st, err := s.ApplyShareChangeContext(ctx, ShareChange{Mode: ShareSelect, Add: chunk(fmt.Sprint(i), 50)})
		if err != nil {
			if !errors.Is(err, ErrInvalidShare) {
				t.Fatalf("step %d: %v", i, err)
			}
			break
		}
		last = st
		if i > 100 {
			t.Fatal("no budget")
		}
	}
	after, err := s.LoadShareStateContext(ctx)
	if err != nil || after.Revision != last.Revision {
		t.Fatalf("the refused add changed the selection: %+v, %v", after, err)
	}
	if enc, _ := json.Marshal(after.Sessions); len(enc) > MaxShareSelectionBytes {
		t.Fatalf("saved selection of %d bytes exceeds the budget", len(enc))
	}
	if st, err := s.ApplyShareChangeContext(ctx, ShareChange{Mode: ShareNone}); err != nil || st.Mode != ShareNone {
		t.Fatalf("set none = %+v, %v", st, err)
	}
}
