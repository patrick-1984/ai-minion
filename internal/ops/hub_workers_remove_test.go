package ops

import (
	"errors"
	"testing"
)

func TestWorkerRemoveIsDeclared(t *testing.T) {
	op, ok := Lookup(OpWorkerRemove)
	if !ok {
		t.Fatal("worker_remove is not registered")
	}
	// operate on that Worker: the owner and admins pass by ownership, not the
	// admin-only manage action (A-8 style).
	if op.Scope != ScopeHub || op.Actions.String() != "operate" || !op.Write || op.Stream {
		t.Errorf("scope %s actions %s write %v stream %v", op.Scope, op.Actions, op.Write, op.Stream)
	}
	if len(op.Guards) != 1 || op.Guards[0].Name != "worker_exists" || op.Guards[0].Class != GuardKeep {
		t.Errorf("guards = %+v", op.Guards)
	}
	if op.RESTRoute() != "POST /api/v1/ops/worker_remove" || op.WireName() != "worker_remove" {
		t.Errorf("derived names: %s, %s", op.RESTRoute(), op.WireName())
	}
}

func TestWorkerRemoveArgsValidate(t *testing.T) {
	ok := WorkerRemoveArgs{WriteArgs: WriteArgs{RequestID: NewUUIDv7()}, WorkerID: NewUUIDv7()}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*WorkerRemoveArgs){
		"no request id": func(a *WorkerRemoveArgs) { a.RequestID = "" },
		"no worker id":  func(a *WorkerRemoveArgs) { a.WorkerID = "" },
		"not a UUID":    func(a *WorkerRemoveArgs) { a.WorkerID = "pi" },
	} {
		a := ok
		mod(&a)
		if err := a.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
