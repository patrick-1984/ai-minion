package agent

import (
	"context"
	"slices"
	"testing"
)

func TestExporterModuleRunsInsideTheLockAndIsJoinedFirst(t *testing.T) {
	names := DefaultRegistry.Names()
	lock, core, exp := slices.Index(names, ModuleLifecycle), slices.Index(names, ModuleCore), slices.Index(names, ModuleExporter)
	if lock < 0 || core < 0 || exp < 0 || (lock >= core || core >= exp) {
		t.Fatalf("default modules %v: the exporter must start after the lifecycle lock and the core", names)
	}
	l := &eventLog{}
	r := NewRegistry()
	fakeModule(t, r, l, "lock", OrderLifecycle)
	if err := r.Register(ModuleExporter, OrderExporter, startExporter); err != nil {
		t.Fatal(err)
	}
	s := testSettings()
	s.StartBackground = func(context.Context) func() {
		l.add("start installer")
		return func() { l.add("stop installer") }
	}
	rt := NewRuntime(r, Options{})
	if err := rt.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if err := rt.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"start lock", "start installer", "stop installer", "stop lock"}
	if got := l.get(); !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
}
