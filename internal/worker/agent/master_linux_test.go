//go:build linux && !android

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

func TestMasterBrokerUsesTheSocketProcess(t *testing.T) {
	if !masterPeerSupported() {
		t.Skip("kernel lacks SO_PEERPIDFD")
	}
	path := filepath.Join(t.TempDir(), "peer")
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	server, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	conn, err := server.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer, err := masterPeer(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.close()
	image, err := os.Stat("/proc/self/exe")
	if err != nil || peer.pid != os.Getpid() || !peer.live() || !peer.imageIs(image) {
		t.Fatalf("the socket peer did not match the actual process: pid %d, err %v", peer.pid, err)
	}
}

func TestMasterLeaseAcknowledgementAndRetirement(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	policy := NewPolicy(nil)
	policy.SetWorkerID("worker")
	policy.SetOwnerID("owner")
	l := &Link{Welcome: HubWelcome{WorkerID: "worker", OwnerID: "owner"}, ctx: ctx, gen: 4}
	b := newMasterBroker(nil, &Executor{policy: policy}, nil)
	lane := &masterLane{ctx: ctx, link: l}
	if !b.openLane(lane) {
		t.Fatal("eligible link refused")
	}
	a := &masterAttachment{terminal: tmux.MasterPane{SessionName: "old"}, incarnation: "incarnation",
		owner: linkMasterOwner(l), harness: &masterProcess{pid: 42, fd: -1}, peer: &masterProcess{fd: -1}}
	b.attachments[a.harness.pid] = a
	state := proto.MasterState{TTLMS: proto.MasterLeaseTTL.Milliseconds(), Active: []proto.MasterBinding{
		{IncarnationID: a.incarnation, Generation: 6, LeaseID: "fixture-lease"},
	}}
	now := time.Now()
	b.applyMasterState(lane, state, sentMasterReport{at: now})
	if a.lease != nil {
		t.Fatal("unreported incarnation was granted a lease")
	}
	ids := map[string]bool{a.incarnation: true}
	sent := sentMasterReport{at: now.Add(-8 * time.Second), ids: ids}
	b.applyMasterState(lane, state, sent)
	lease := a.lease
	if lease == nil || !lease.until.Equal(sent.at.Add(proto.MasterLeaseTTL)) {
		t.Fatal("lease did not use the monotonic report-send deadline")
	}
	b.applyMasterState(lane, state, sent)
	if a.lease != lease || !lease.until.Equal(sent.at.Add(proto.MasterLeaseTTL)) {
		t.Fatal("an unsolicited state extended the acknowledged deadline")
	}
	b.applyMasterState(lane, state, sentMasterReport{at: now.Add(-proto.MasterLeaseTTL), ids: ids})
	if a.lease != nil || !errors.Is(lease.ctx.Err(), context.Canceled) {
		t.Fatal("an expired acknowledgement retained authority")
	}
	b.applyMasterState(lane, state, sent)
	replacement := a.lease
	b.revokeUnshared(store.ShareSelection{Mode: store.ShareNone})
	if len(b.attachments) != 0 || !errors.Is(replacement.ctx.Err(), context.Canceled) {
		t.Fatal("unsharing left an attachment or in-flight authority")
	}
}

func TestScopedMasterLocalWriteRequiresACompleteOrdinaryPrompt(t *testing.T) {
	id, worker := ops.NewUUIDv7(), ops.NewUUIDv7()
	request := localapi.MasterRequest{Generation: 4, RequestID: id, Operation: ops.OpSendText}
	valid := ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: id},
		Target: ops.Target{WorkerID: worker, SessionName: "agent"}, Text: "Fixture prompt", Submit: true}
	request.Arguments, _ = json.Marshal(valid)
	if err := validateLocalMasterCall(request); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ops.SendTextArgs){
		func(a *ops.SendTextArgs) { a.Force = true },
		func(a *ops.SendTextArgs) { a.Submit = false },
		func(a *ops.SendTextArgs) { a.AgentOnly = true },
		func(a *ops.SendTextArgs) { a.RequestID = ops.NewUUIDv7() },
		func(a *ops.SendTextArgs) { a.Target.WorkerID = "" },
	} {
		bad := valid
		change(&bad)
		request.Arguments, _ = json.Marshal(bad)
		if err := validateLocalMasterCall(request); err == nil {
			t.Fatal("invalid scoped prompt was accepted")
		}
	}
}
