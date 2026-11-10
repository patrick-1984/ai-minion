package server

import (
	"context"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

func TestAuditModuleProvidesTheLogFromTheStore(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	r := NewRegistry()
	if err := r.Register("store", 10, func(_ context.Context, env *Env) error {
		return Provide(env, st)
	}, DeclaresService[*store.Store]()); err != nil {
		t.Fatal(err)
	}
	registerAuditModule(r)
	k := NewKernel(r)
	if err := k.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	log, ok := Lookup[*audit.Log](k)
	if !ok {
		t.Fatal("the audit module did not provide *audit.Log")
	}
	if err := log.Record(context.Background(), audit.Event{Actor: "a", Action: "login", Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	p, err := log.List(context.Background(), audit.Filter{})
	if err != nil || len(p.Events) != 1 {
		t.Fatalf("events = %d, err %v", len(p.Events), err)
	}
}

func TestAuditModuleNamesTheMissingStore(t *testing.T) {
	r := NewRegistry()
	registerAuditModule(r)
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"audit"`) || !strings.Contains(err.Error(), "store.Store") {
		t.Errorf("err = %v", err)
	}
}

func TestAuditModuleIsRegisteredInTheDefaultRegistry(t *testing.T) {
	found := false
	for _, n := range DefaultRegistry.Names() {
		found = found || n == "audit"
	}
	if !found {
		t.Errorf("modules = %v", DefaultRegistry.Names())
	}
}
