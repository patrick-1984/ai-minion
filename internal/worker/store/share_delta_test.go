package store

import (
	"context"
	"errors"
	"slices"
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
