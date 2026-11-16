package agent

import (
	"context"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

type selSource struct{ sel store.ShareSelection }

func (s selSource) LoadShareContext(context.Context) (store.ShareSelection, error) { return s.sel, nil }

// On a host whose pane ids repeat across sessions (psmux), a selection of
// single sessions shares nothing; all and none keep their meaning.
func TestAllOrNoneTurnsASelectionIntoNone(t *testing.T) {
	ctx := context.Background()
	p := NewPolicy(selSource{store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"a"}, Configured: true}})
	if ok, _ := p.Shared(ctx, "a"); !ok {
		t.Fatal("without the limit a selected session is shared")
	}
	p.SetAllOrNone(true)
	if ok, _ := p.Shared(ctx, "a"); ok {
		t.Fatal("with the limit a selected session must not be shared")
	}
	p = NewPolicy(selSource{store.ShareSelection{Mode: store.ShareAll, Configured: true}})
	p.SetAllOrNone(true)
	if ok, _ := p.Shared(ctx, "b"); !ok {
		t.Fatal("all stays all")
	}
}
