package ops

import "testing"

func TestWorkerRetireIsDeclaredOnceAsAHubWrite(t *testing.T) {
	op, ok := Lookup(OpWorkerRetire)
	if !ok {
		t.Fatal("worker_retire is not in the catalog")
	}
	if op.Scope != ScopeHub || !op.Write || op.Stream || op.Actions.String() != "operate" {
		t.Errorf("scope %s write %v stream %v actions %s", op.Scope, op.Write, op.Stream, op.Actions)
	}
	if len(op.Guards) != 1 || op.Guards[0].Class != GuardKeep {
		t.Errorf("guards = %+v, want one KEEP guard", op.Guards)
	}
	if op.WireName() != "worker_retire" {
		t.Errorf("wire name %q", op.WireName())
	}
}
