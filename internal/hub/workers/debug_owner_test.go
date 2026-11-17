package workers

import (
	"context"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Disconnect records the Workers whose connection the Router closed.
func (b *fakeBackend) Disconnect(workerID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.disconnected = append(b.disconnected, workerID)
	_, ok := b.online[workerID]
	return ok
}

// reassignBoxToAlice is the committed reassignment: the database says alice
// owns box from now on.
func reassignBoxToAlice(r *rig) {
	for i := range r.dir.recs {
		if r.dir.recs[i].ID == wBobBox.ID {
			r.dir.recs[i].OwnerID = idAlice
		}
	}
}

func debugReqs() map[string]CallRequest {
	return map[string]CallRequest{
		ops.OpWorkerLogs:  {Principal: routeAlice(), Op: ops.OpWorkerLogs, WorkerID: wBobBox.ID, Args: ops.WorkerLogsArgs{WorkerID: wBobBox.ID}},
		ops.OpWorkerDebug: {Principal: routeAlice(), Op: ops.OpWorkerDebug, WorkerID: wBobBox.ID, Args: ops.WorkerDebugArgs{WorkerID: wBobBox.ID}},
	}
}

// The race of the review: a reconnect read the previous owner (bob) before
// the reassignment to alice committed, and registered after it disconnected
// the old link. Its welcome told the Worker bob, so the Worker still holds
// bob's log. The new owner is refused on that connection, nothing is sent,
// and the stale connection is closed so that the Worker reconnects.
func TestDebugOnAConnectionOfThePreviousOwnerIsRefused(t *testing.T) {
	for op, req := range debugReqs() {
		t.Run(op, func(t *testing.T) {
			r := newRig(t, nil)
			reassignBoxToAlice(r)
			r.be.online[wBobBox.ID] = Online{WorkerID: wBobBox.ID, OwnerID: idBob, Ops: []string{ops.OpWorkerDebug, ops.OpWorkerLogs}}
			r.script(newFakeStream(resultFrame(t, ops.WorkerLogsResult{})))
			_, err := r.rt.Call(context.Background(), req)
			if code(err) != proto.CodeConflict || !strings.Contains(err.Error(), "reconnecting after an owner change; try again") {
				t.Fatalf("err = %v, want the stale-owner refusal", err)
			}
			if r.be.openCount() != 0 {
				t.Errorf("the stale connection got the op: %v", r.be.opens)
			}
			if len(r.be.disconnected) != 1 || r.be.disconnected[0] != wBobBox.ID {
				t.Errorf("disconnected = %v, want the stale connection closed", r.be.disconnected)
			}
		})
	}
}

// The stale connection registers between the Router's read of the owner and
// the open: the check runs on the connection that gets the op.
func TestDebugOwnerIsJudgedOnTheConnectionThatGetsIt(t *testing.T) {
	r := newRig(t, nil)
	reassignBoxToAlice(r)
	r.be.online[wBobBox.ID] = Online{WorkerID: wBobBox.ID, OwnerID: idAlice}
	r.be.beforeOpen = func(b *fakeBackend) {
		b.online[wBobBox.ID] = Online{WorkerID: wBobBox.ID, OwnerID: idBob}
	}
	r.script(newFakeStream(resultFrame(t, ops.WorkerLogsResult{})))
	_, err := r.rt.Call(context.Background(), debugReqs()[ops.OpWorkerLogs])
	if code(err) != proto.CodeConflict {
		t.Fatalf("err = %v, want the stale-owner refusal", err)
	}
	if r.be.openCount() != 0 {
		t.Errorf("the stale connection got the op: %v", r.be.opens)
	}
}

// A connection welcomed under the current owner serves the op; other ops are
// not affected by a stale owner (they carry no previous owner's data).
func TestDebugOnAConnectionOfTheCurrentOwnerIsSent(t *testing.T) {
	r := newRig(t, nil)
	reassignBoxToAlice(r)
	r.be.online[wBobBox.ID] = Online{WorkerID: wBobBox.ID, OwnerID: idAlice}
	r.script(newFakeStream(resultFrame(t, ops.WorkerLogsResult{})))
	if _, err := r.rt.Call(context.Background(), debugReqs()[ops.OpWorkerLogs]); err != nil {
		t.Fatalf("worker_logs: %v", err)
	}
	if r.be.openCount() != 1 || len(r.be.disconnected) != 0 {
		t.Errorf("opens = %v, disconnected = %v", r.be.opens, r.be.disconnected)
	}
	r.be.online[wBobBox.ID] = Online{WorkerID: wBobBox.ID, OwnerID: idBob}
	r.script(newFakeStream(resultFrame(t, sessionsResult())))
	if _, err := r.rt.Call(context.Background(), CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: wBobBox.ID, Args: ops.SessionsListArgs{}}); err != nil {
		t.Fatalf("sessions_list: %v", err)
	}
}
