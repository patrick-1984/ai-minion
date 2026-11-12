package ops

import "testing"

func TestWorkerPairingReissueIsAWriteOnTheWorker(t *testing.T) {
	op, ok := Lookup(OpWorkerPairingReissue)
	if !ok {
		t.Fatal("worker_pairing_reissue is not registered")
	}
	if !op.Write || op.Scope != ScopeHub || op.Actions.String() != "operate" {
		t.Errorf("op = %+v", op)
	}
	if op.RESTRoute() != "POST /api/v1/ops/worker_pairing_reissue" {
		t.Errorf("route = %q", op.RESTRoute())
	}
	bad := WorkerPairingReissueArgs{WriteArgs: WriteArgs{RequestID: NewUUIDv7()}, WorkerID: "nope"}
	if err := bad.Validate(); err == nil {
		t.Error("a malformed worker id was accepted")
	}
	good := WorkerPairingReissueArgs{WriteArgs: WriteArgs{RequestID: NewUUIDv7()}, WorkerID: NewUUIDv7()}
	if err := good.Validate(); err != nil {
		t.Errorf("valid args refused: %v", err)
	}
}
