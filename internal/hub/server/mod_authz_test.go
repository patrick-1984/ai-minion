package server

import (
	"context"
	"errors"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

type nopUsers struct{}

func (nopUsers) User(context.Context, string) (authz.UserState, error) {
	return authz.UserState{Role: authz.RoleUser, Active: true}, nil
}

type nopWorkers struct{}

func (nopWorkers) WorkerOwner(context.Context, string) (string, error) { return "alice", nil }

func startAuthz(t *testing.T) *authz.Authorizer {
	t.Helper()
	r := NewRegistry()
	registerAuthz(r)
	k := NewKernel(r)
	if err := k.Start(context.Background()); err != nil {
		t.Fatalf("a kernel with only the authz module must start: %v", err)
	}
	a, ok := Lookup[*authz.Authorizer](k)
	if !ok || a == nil {
		t.Fatal("no *authz.Authorizer service")
	}
	return a
}

func TestAuthzModuleStartsWithoutLookupProviders(t *testing.T) {
	startAuthz(t)
}

func TestAuthzModuleFailsClosedBeforeWiring(t *testing.T) {
	a := startAuthz(t)
	ctx := context.Background()
	p := authz.NewSessionPrincipal("alice")
	res := authz.WorkerResource{ID: "w-alice"}
	read := ops.MustActionSet(ops.ActionRead)

	if err := a.Decide(ctx, p, read, res); !errors.Is(err, authz.ErrNotWired) {
		t.Errorf("Decide before wiring = %v, want ErrNotWired", err)
	}
	if _, err := a.EffectiveActions(ctx, p, res); !errors.Is(err, authz.ErrNotWired) {
		t.Errorf("EffectiveActions before wiring = %v, want ErrNotWired", err)
	}
	items := []string{"w-alice"}
	got, err := authz.Filter(ctx, a, p, items, func(id string) authz.Resource { return authz.WorkerResource{ID: id} })
	if !errors.Is(err, authz.ErrNotWired) || len(got) != 0 {
		t.Errorf("Filter before wiring = %v, %v", got, err)
	}
	// Only the user port wired: the Worker port still fails closed.
	if err := a.SetUserLookup(nopUsers{}); err != nil {
		t.Fatal(err)
	}
	if err := a.Decide(ctx, p, read, res); !errors.Is(err, authz.ErrNotWired) {
		t.Errorf("Decide with only users wired = %v, want ErrNotWired", err)
	}
}

func TestAuthzModuleDecidesAfterWiring(t *testing.T) {
	a := startAuthz(t)
	if err := a.SetUserLookup(nopUsers{}); err != nil {
		t.Fatal(err)
	}
	if err := a.SetWorkerLookup(nopWorkers{}); err != nil {
		t.Fatal(err)
	}
	err := a.Decide(context.Background(), authz.NewSessionPrincipal("alice"), ops.MustActionSet(ops.ActionRead), authz.WorkerResource{ID: "w-alice"})
	if err != nil {
		t.Errorf("Decide after wiring = %v", err)
	}
}

func TestAuthzModuleRefusesSecondWiring(t *testing.T) {
	a := startAuthz(t)
	if err := a.SetUserLookup(nopUsers{}); err != nil {
		t.Fatal(err)
	}
	if err := a.SetUserLookup(nopUsers{}); err == nil {
		t.Error("second SetUserLookup was accepted")
	}
	if err := a.SetWorkerLookup(nopWorkers{}); err != nil {
		t.Fatal(err)
	}
	if err := a.SetWorkerLookup(nopWorkers{}); err == nil {
		t.Error("second SetWorkerLookup was accepted")
	}
	if err := a.SetUserLookup(nil); err == nil {
		t.Error("nil lookup accepted")
	}
}

func TestAuthzModuleIsInDefaultRegistry(t *testing.T) {
	found := false
	for _, n := range DefaultRegistry.Names() {
		found = found || n == "authz"
	}
	if !found {
		t.Fatalf("authz not registered in DefaultRegistry: %v", DefaultRegistry.Names())
	}
}

func TestDefaultRegistryStartsWithAuthz(t *testing.T) {
	// A fresh registry holding only the authz module: the default registry
	// also holds modules (audit) that need the store module of story 3.2.
	r := NewRegistry()
	registerAuthz(r)
	k := NewKernel(r)
	if err := k.Start(context.Background()); err != nil {
		t.Fatalf("the authz module must start without lookup providers: %v", err)
	}
	if _, ok := Lookup[*authz.Authorizer](k); !ok {
		t.Fatal("authz module does not provide *authz.Authorizer")
	}
}
