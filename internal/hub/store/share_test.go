package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The share rule of every Worker is kept durably, replaced by a newer one, and
// forgotten with the Worker; a rule without a revision is refused.
func TestWorkerShareRulesAreDurable(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if m, err := s.LoadWorkerShares(ctx); err != nil || len(m) != 0 {
		t.Fatalf("empty store: %v %v", m, err)
	}
	a := ops.ShareDecl{Rule: ops.ShareRule{Mode: "select", Names: []string{"a", "b"}}, Revision: 3}
	if err := s.SaveWorkerShare(ctx, "w1", a); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveWorkerShare(ctx, "w2", ops.ShareDecl{Rule: ops.ShareRule{Mode: "none"}, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	m, err := s.LoadWorkerShares(ctx)
	if err != nil || !reflect.DeepEqual(m["w1"], a) || m["w2"].Rule.Mode != "none" || len(m["w2"].Rule.Names) != 0 {
		t.Fatalf("stored = %+v %v", m, err)
	}
	b := ops.ShareDecl{Rule: ops.ShareRule{Mode: "all"}, Revision: 4}
	if err := s.SaveWorkerShare(ctx, "w1", b); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.LoadWorkerShares(ctx); !reflect.DeepEqual(m["w1"], ops.ShareDecl{Rule: ops.ShareRule{Mode: "all"}, Revision: 4}) {
		t.Fatalf("replaced = %+v", m["w1"])
	}
	if err := s.SaveWorkerShare(ctx, "w1", ops.ShareDecl{Rule: ops.ShareRule{Mode: "all"}}); err == nil {
		t.Fatal("a rule without a revision was stored")
	}
	if err := s.SaveWorkerShare(ctx, "w3", ops.ShareDecl{Rule: ops.ShareRule{Mode: "sometimes"}, Revision: 1}); err == nil {
		t.Fatal("an unknown mode was stored")
	}
	if err := s.DeleteWorkerShare(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.LoadWorkerShares(ctx); len(m) != 1 || m["w2"].Revision != 1 {
		t.Fatalf("after delete: %+v", m)
	}
}
