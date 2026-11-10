package api

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func noop(context.Context, *Call) (any, error) { return struct{}{}, nil }

func TestHandleChecksTheCatalog(t *testing.T) {
	r := NewRegistry(testCatalog(t))
	cases := []struct {
		name    string
		do      func() error
		wantErr string
	}{
		{"unknown op", func() error { return r.Handle("no_such_op", noop) }, "not an operation of the catalog"},
		{"nil handler", func() error { return r.Handle(opPing, nil) }, "is nil"},
		{"stream op with Handle", func() error { return r.Handle(opTick, noop) }, "HandleStream"},
		{"plain op with HandleStream", func() error {
			return r.HandleStream(opPing, func(context.Context, *Call, func(any) error) error { return nil })
		}, "Handle"},
		{"nil stream handler", func() error { return r.HandleStream(opTick, nil) }, "is nil"},
		{"ok", func() error { return r.Handle(opPing, noop) }, ""},
		{"duplicate", func() error { return r.Handle(opPing, noop) }, "already has a handler"},
	}
	for _, c := range cases {
		err := c.do()
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.wantErr)
		}
	}
	if got := r.Names(); !slices.Equal(got, []string{opPing}) {
		t.Errorf("Names = %v", got)
	}
}

func TestHandleTypedChecksTypes(t *testing.T) {
	r := NewRegistry(testCatalog(t))
	if err := HandleTyped(r, opPing, func(_ context.Context, _ *Call, a pingArgs) (pingResult, error) {
		return pingResult{Echo: a.Message}, nil
	}); err != nil {
		t.Fatal(err)
	}
	err := HandleTyped(r, opShareGet, func(_ context.Context, _ *Call, a pingArgs) (pingResult, error) { return pingResult{}, nil })
	if err == nil || !strings.Contains(err.Error(), "takes") {
		t.Errorf("wrong args type accepted: %v", err)
	}
	err = HandleTyped(r, opShareGet, func(_ context.Context, _ *Call, a shareArgs) (pingResult, error) { return pingResult{}, nil })
	if err == nil || !strings.Contains(err.Error(), "returns") {
		t.Errorf("wrong result type accepted: %v", err)
	}
	err = HandleTyped(r, "no_such_op", func(_ context.Context, _ *Call, a shareArgs) (shareResult, error) { return shareResult{}, nil })
	if err == nil {
		t.Error("unknown op accepted")
	}
}

func TestMerge(t *testing.T) {
	cat := testCatalog(t)
	a, b := NewRegistry(cat), NewRegistry(cat)
	mustHandle(t, a, opPing, noop)
	mustHandle(t, b, opShareGet, noop)
	if err := b.Merge(a); err != nil {
		t.Fatal(err)
	}
	if len(b.Names()) != 2 {
		t.Errorf("Names = %v", b.Names())
	}
	// A duplicate fails and copies nothing.
	c := NewRegistry(cat)
	mustHandle(t, c, opPing, noop)
	d := NewRegistry(cat)
	mustHandle(t, d, opPing, noop)
	mustHandle(t, d, opShareGet, noop)
	if err := c.Merge(d); err == nil || len(c.Names()) != 1 {
		t.Errorf("Merge with a duplicate: err = %v, names = %v", err, c.Names())
	}
	// An op the target catalog does not know fails.
	other := NewRegistry(ops.NewRegistry())
	if err := other.Merge(a); err == nil {
		t.Error("Merge into a registry with another catalog succeeded")
	}
}

func TestPackageHandleRegistersInDefaultAndPanicsOnError(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Handle of an unknown op did not panic")
		}
	}()
	Handle("definitely_not_an_op", noop)
}
