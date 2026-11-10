package ops

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWorkersListIsAHubReadOp(t *testing.T) {
	op, ok := Lookup(OpWorkersList)
	if !ok {
		t.Fatal("workers_list is not registered")
	}
	if op.Scope != ScopeHub || op.Write || op.Stream || op.Actions.String() != "read" {
		t.Errorf("op = %+v", op)
	}
	if op.RESTPath() != "/api/v1/ops/workers_list" || strings.Join(op.RESTMethods(), ",") != "POST,GET" {
		t.Errorf("route = %s %v", op.RESTPath(), op.RESTMethods())
	}
	if op.CLICommand() != "workers_list" || op.MCPTool() != "workers_list" {
		t.Error("derived names differ")
	}
}

func TestWorkersListWireForm(t *testing.T) {
	at := Timestamp(time.Date(2026, 11, 9, 12, 0, 0, 123_000_000, time.UTC))
	res := WorkersListResult{Workers: []WorkerInfo{
		{ID: "w1", Name: "pi-1", OwnerID: "u1", Owner: "alice", Kind: WorkerKindPrivate, Online: true, ProtoVersion: "1.0", ConnectedAt: &at, Ops: []string{"send_text"}},
		{ID: "w2", Name: "box", OwnerID: "u2", Owner: "bob", Kind: WorkerKindInfrastructure},
	}}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"workers":[` +
		`{"id":"w1","name":"pi-1","owner_id":"u1","owner":"alice","kind":"private","online":true,"proto_version":"1.0","connected_at":"2026-11-09T12:00:00.123Z","ops":["send_text"]},` +
		`{"id":"w2","name":"box","owner_id":"u2","owner":"bob","kind":"infrastructure","online":false}]}`
	if string(b) != want {
		t.Errorf("wire form\n got %s\nwant %s", b, want)
	}
	var back WorkersListResult
	if err := json.Unmarshal(b, &back); err != nil || len(back.Workers) != 2 || !back.Workers[0].ConnectedAt.Time().Equal(at.Time()) {
		t.Errorf("round trip: %+v (%v)", back, err)
	}
}

func TestWorkersListArgsDefaultToMine(t *testing.T) {
	var a WorkersListArgs
	if err := json.Unmarshal([]byte(`{}`), &a); err != nil || a.All {
		t.Errorf("default args = %+v (%v)", a, err)
	}
	if err := json.Unmarshal([]byte(`{"all":true,"future":1}`), &a); err != nil || !a.All {
		t.Errorf("all=true args = %+v (%v)", a, err)
	}
	if b, _ := json.Marshal(WorkersListArgs{}); string(b) != `{}` {
		t.Errorf("empty args encode as %s", b)
	}
}

func TestWorkerKindsMatchTheStoredValues(t *testing.T) {
	if WorkerKindPrivate != "private" || WorkerKindInfrastructure != "infrastructure" {
		t.Error("worker kind constants changed")
	}
}
