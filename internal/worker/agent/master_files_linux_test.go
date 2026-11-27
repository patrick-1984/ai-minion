//go:build linux && !android

package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

func TestMasterFileCallerMustRunBelowTheHarness(t *testing.T) {
	if !descendsFrom(os.Getpid(), os.Getppid()) || !descendsFrom(os.Getpid(), os.Getpid()) {
		t.Fatal("the test process does not run below its parent")
	}
	if descendsFrom(os.Getppid(), os.Getpid()) || descendsFrom(0, os.Getpid()) {
		t.Fatal("a parent runs below its child")
	}
}

func TestMasterFileCallWithoutLeaseIsNotHandled(t *testing.T) {
	policy := NewPolicy(nil)
	policy.SetWorkerID("worker")
	policy.SetOwnerID("owner")
	b := newMasterBroker(nil, &Executor{policy: policy}, nil)
	var out ops.FileTransferResult
	for _, pid := range []int{0, os.Getpid()} {
		handled, err := b.fileCall(t.Context(), "boss", pid, ops.OpFilePull, ops.FilePullArgs{}, &out)
		if handled || err != nil {
			t.Fatalf("pid %d without an attachment: handled %v, %v", pid, handled, err)
		}
	}
}

// shareEverything shares every session.
type shareEverything struct{}

func (shareEverything) LoadShareContext(context.Context) (store.ShareSelection, error) {
	return store.ShareSelection{Mode: store.ShareAll}, nil
}

// masterAppFixture is a broker with a lane on a fake Hub that answers every
// call with result, and an attachment of the session boss whose harness is
// this test process, with a current lease.
func masterAppFixture(t *testing.T, welcome HubWelcome, result any) (*masterBroker, *callHub) {
	t.Helper()
	h := startCallHub(t, func(st *transport.Stream, _ map[string]json.RawMessage) {
		reply(st, map[string]any{"type": proto.TypeOpResult, "result": result})
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	policy := NewPolicy(shareEverything{})
	policy.SetWorkerID("worker")
	policy.SetOwnerID("owner")
	welcome.WorkerID, welcome.OwnerID = "worker", "owner"
	l := &Link{Session: h.worker, Welcome: welcome, ctx: ctx, gen: 1}
	b := newMasterBroker(nil, &Executor{policy: policy}, nil)
	if !b.openLane(&masterLane{ctx: ctx, link: l}) {
		t.Fatal("eligible link refused")
	}
	a := &masterAttachment{terminal: tmux.MasterPane{SessionName: "boss"}, incarnation: "incarnation",
		owner: linkMasterOwner(l), harness: &masterProcess{pid: os.Getpid(), fd: -1}, peer: &masterProcess{fd: -1}}
	leaseCtx, leaseCancel := context.WithCancel(ctx)
	t.Cleanup(leaseCancel)
	a.lease = &masterLease{wire: proto.MasterBinding{IncarnationID: a.incarnation, Generation: 3, LeaseID: "fixture-lease"},
		until: time.Now().Add(proto.MasterLeaseTTL), ctx: leaseCtx, cancel: leaseCancel}
	b.attachments[a.harness.pid] = a
	return b, h
}

func TestMasterAppCallGoesOnTheLane(t *testing.T) {
	b, h := masterAppFixture(t, HubWelcome{MasterSessionV1: true, AppInstallV1: true},
		ops.AppInstallResult{Outcome: ops.AppInstallInstalled, Package: "org.example.app"})
	other := ops.NewUUIDv7()
	var res ops.AppInstallResult
	handled, err := b.appCall(t.Context(), "boss", os.Getpid(), ops.OpAppInstall, ops.AppInstallArgs{WorkerID: other, Path: "x.apk"}, &res)
	if !handled || err != nil || res.Outcome != ops.AppInstallInstalled {
		t.Fatalf("appCall = %v, %+v, %v", handled, res, err)
	}
	req := h.requestAt(0)
	var args proto.MasterCallArgs
	if string(req["op"]) != `"`+proto.OpMasterCallV1+`"` || json.Unmarshal(req["args"], &args) != nil ||
		args.Operation != ops.OpAppInstall || args.LeaseID != "fixture-lease" || args.Generation != 3 || !strings.Contains(string(args.Arguments), other) {
		t.Fatalf("the Hub got %v", req)
	}
	// Another session, or a process that does not run below the harness,
	// is not served by the lane.
	for _, c := range []struct {
		session string
		pid     int
	}{{"other", os.Getpid()}, {"boss", os.Getppid()}} {
		if handled, err := b.appCall(t.Context(), c.session, c.pid, ops.OpAppInstall, ops.AppInstallArgs{WorkerID: other, Path: "x.apk"}, &res); handled || err != nil {
			t.Fatalf("%s/%d: handled %v, %v", c.session, c.pid, handled, err)
		}
	}
}

func TestMasterAppCallNeedsAHubThatRoutesAppsOnTheLane(t *testing.T) {
	// A v0.24 Hub (no app_install_v1) serves file ops on the lane, not app
	// installs: the caller uses the ordinary call stream.
	b, h := masterAppFixture(t, HubWelcome{MasterSessionV1: true, FilesV1: true}, ops.FileListResult{})
	var res json.RawMessage
	if handled, err := b.appCall(t.Context(), "boss", os.Getpid(), ops.OpAppInstallStatus, ops.AppInstallStatusArgs{WorkerID: ops.NewUUIDv7()}, &res); handled || err != nil {
		t.Fatalf("appCall without app_install_v1: handled %v, %v", handled, err)
	}
	if handled, err := b.fileCall(t.Context(), "boss", os.Getpid(), ops.OpFileList, ops.FileListArgs{Target: ops.Target{WorkerID: ops.NewUUIDv7(), SessionName: "b"}}, &res); !handled || err != nil {
		t.Fatalf("fileCall with files_v1: handled %v, %v", handled, err)
	}
	h.mu.Lock()
	n := len(h.requests)
	h.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d requests reached the Hub, want the file list only", n)
	}
}
