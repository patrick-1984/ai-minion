package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

type shareStub struct {
	sel store.ShareSelection
	err error
}

func (s shareStub) LoadShareContext(context.Context) (store.ShareSelection, error) {
	return s.sel, s.err
}

func TestPolicyShared(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		sel  store.ShareSelection
		name string
		want bool
	}{
		{store.ShareSelection{Mode: store.ShareAll}, "x", true},
		{store.ShareSelection{Mode: store.ShareNone}, "x", false},
		{store.ShareSelection{Mode: store.ShareLater}, "x", false},
		{store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"a"}}, "a", true},
		{store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"a"}}, "b", false},
	}
	for _, c := range cases {
		got, err := NewPolicy(shareStub{sel: c.sel}).Shared(ctx, c.name)
		if err != nil || got != c.want {
			t.Errorf("%+v %q = %v, %v; want %v", c.sel, c.name, got, err, c.want)
		}
	}
}

func TestPolicyWithoutSourceSharesNothingAndPropagatesErrors(t *testing.T) {
	ctx := context.Background()
	if ok, err := NewPolicy(nil).Shared(ctx, "x"); ok || err != nil {
		t.Fatalf("nil source: %v, %v", ok, err)
	}
	boom := errors.New("boom")
	if _, err := NewPolicy(shareStub{err: boom}).Shared(ctx, "x"); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestPolicyPreviewOnlyToggles(t *testing.T) {
	p := NewPolicy(nil)
	if p.PreviewOnly() {
		t.Fatal("preview-only must default to off")
	}
	p.SetPreviewOnly(true)
	if !p.PreviewOnly() {
		t.Fatal("not set")
	}
	p.SetPreviewOnly(false)
	if p.PreviewOnly() {
		t.Fatal("not cleared")
	}
}
