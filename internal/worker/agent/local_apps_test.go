package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// v0.25.0: a local app_install or app_install_status that names another
// Worker goes to the Hub: on the master lane for a verified calling session,
// on an ordinary call stream (a claimed source) otherwise.

func TestForwardsAppOnlyForAnotherWorker(t *testing.T) {
	self, other := ops.NewUUIDv7(), ops.NewUUIDv7()
	w := func(id string) json.RawMessage {
		return mustJSON(t, map[string]string{"worker_id": id, "path": "x.apk"})
	}
	for _, tc := range []struct {
		name string
		op   string
		args json.RawMessage
		self string
		want bool
	}{
		{"install on another Worker", ops.OpAppInstall, w(other), self, true},
		{"status of another Worker", ops.OpAppInstallStatus, w(other), self, true},
		{"install without an own id", ops.OpAppInstall, w(other), "", true},
		{"install on this Worker", ops.OpAppInstall, w(self), self, false},
		{"install without a Worker", ops.OpAppInstall, mustJSON(t, map[string]string{"path": "x.apk"}), self, false},
		{"another op", ops.OpFileList, w(other), self, false},
	} {
		if got := forwardsApp(tc.op, tc.args, tc.self); got != tc.want {
			t.Errorf("%s: forwardsApp = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// appRequest is a local app op of the pane pane, capability-qualified as the
// current client sends it.
func localAppRequest(t *testing.T, op string, args any, pane string, pid int) localapi.Request {
	t.Helper()
	raw := mustJSON(t, args)
	rq := localapiRequest(localapi.QualifiedOp(op, ops.LocalRequiredFeatures(op, raw)), raw)
	rq.Pane, rq.PeerPID = pane, pid
	return rq
}

func TestAppInstallOfAnotherWorkerGoesOnTheMasterLane(t *testing.T) {
	e := newEnv(t)
	ref := e.host.addSession("boss", "claude")
	calls := &fakePipeCaller{doFn: func(string, any) (json.RawMessage, error) {
		t.Error("the call stream was used although the master lane served the call")
		return nil, nil
	}}
	type laneCall struct {
		session, op string
		pid         int
		args        any
	}
	var got []laneCall
	lane := func(_ context.Context, session string, pid int, op string, args any, out any) (bool, error) {
		got = append(got, laneCall{session, op, pid, args})
		res := ops.AppInstallResult{Outcome: ops.AppInstallInstalled, Package: "org.example.app"}
		if op == ops.OpAppInstallStatus {
			*(out.(*json.RawMessage)) = json.RawMessage(`{"ready":true}`)
			return true, nil
		}
		*(out.(*json.RawMessage)) = mustJSON(t, res)
		return true, nil
	}
	l := localExecutor{ex: e.ex, log: discardLogger(), calls: func() PipeCaller { return calls },
		masterApps: func() masterFiles { return lane }}
	other := ops.NewUUIDv7()
	raw, err := l.Execute(t.Context(), localAppRequest(t, ops.OpAppInstall, ops.AppInstallArgs{WorkerID: other, Path: "x.apk", AllowNew: true}, ref.ID, 42), func(json.RawMessage) {})
	if err != nil {
		t.Fatal(err)
	}
	var res ops.AppInstallResult
	if json.Unmarshal(raw, &res) != nil || res.Outcome != ops.AppInstallInstalled {
		t.Fatalf("result = %s", raw)
	}
	if _, err := l.Execute(t.Context(), localAppRequest(t, ops.OpAppInstallStatus, ops.AppInstallStatusArgs{WorkerID: other}, ref.ID, 42), func(json.RawMessage) {}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].session != "boss" || got[0].pid != 42 || got[0].op != ops.OpAppInstall || got[1].op != ops.OpAppInstallStatus {
		t.Fatalf("lane calls = %+v", got)
	}
	if a, ok := got[0].args.(ops.AppInstallArgs); !ok || a.WorkerID != other || a.Path != "x.apk" || !a.AllowNew {
		t.Fatalf("lane args = %#v", got[0].args)
	}
}

func TestAppInstallWithoutAVerifiedLaneIsAClaimedCall(t *testing.T) {
	e := newEnv(t)
	ref := e.host.addSession("boss", "claude")
	calls := &fakePipeCaller{doFn: func(string, any) (json.RawMessage, error) {
		return nil, proto.NewError(proto.CodeForbidden, "a session installs apps only with a verified source")
	}}
	lanes := 0
	notServed := func(context.Context, string, int, string, any, any) (bool, error) { lanes++; return false, nil }
	other := ops.NewUUIDv7()
	for name, l := range map[string]localExecutor{
		"lane declines": {ex: e.ex, log: discardLogger(), calls: func() PipeCaller { return calls }, masterApps: func() masterFiles { return notServed }},
		"no lane":       {ex: e.ex, log: discardLogger(), calls: func() PipeCaller { return calls }},
	} {
		before := calls.count()
		_, err := l.Execute(t.Context(), localAppRequest(t, ops.OpAppInstall, ops.AppInstallArgs{WorkerID: other, Path: "x.apk"}, ref.ID, 42), func(json.RawMessage) {})
		if proto.CodeOf(err) != proto.CodeForbidden || !strings.Contains(err.Error(), "verified source") {
			t.Fatalf("%s: err = %v", name, err)
		}
		if calls.count() != before+1 {
			t.Fatalf("%s: the call stream was not used", name)
		}
		c := calls.calls[len(calls.calls)-1]
		if c.Op != ops.OpAppInstall || !strings.Contains(c.Args, other) {
			t.Fatalf("%s: call = %+v", name, c)
		}
	}
	if lanes != 1 {
		t.Fatalf("the lane was asked %d times", lanes)
	}
	// Without a pid from the kernel the lane is not asked at all.
	l := localExecutor{ex: e.ex, log: discardLogger(), calls: func() PipeCaller { return calls }, masterApps: func() masterFiles { return notServed }}
	if _, err := l.Execute(t.Context(), localAppRequest(t, ops.OpAppInstall, ops.AppInstallArgs{WorkerID: other, Path: "x.apk"}, ref.ID, 0), func(json.RawMessage) {}); proto.CodeOf(err) != proto.CodeForbidden {
		t.Fatalf("without a pid: %v", err)
	}
	if lanes != 1 {
		t.Fatal("the lane was asked without a pid")
	}
}

func TestAppInstallForwardRefusesBadArgumentsAndNoHub(t *testing.T) {
	e := newEnv(t)
	calls := &fakePipeCaller{doFn: func(string, any) (json.RawMessage, error) {
		t.Error("a refused request reached the Hub")
		return nil, nil
	}}
	l := localExecutor{ex: e.ex, log: discardLogger(), calls: func() PipeCaller { return calls }}
	other := ops.NewUUIDv7()
	if _, err := l.Execute(t.Context(), localAppRequest(t, ops.OpAppInstall, ops.AppInstallArgs{WorkerID: other, Path: "../x.apk"}, "", 0), func(json.RawMessage) {}); proto.CodeOf(err) != proto.CodeInvalidArgument {
		t.Fatalf("a path outside Minion files: %v", err)
	}
	offline := localExecutor{ex: e.ex, log: discardLogger()}
	if _, err := offline.Execute(t.Context(), localAppRequest(t, ops.OpAppInstall, ops.AppInstallArgs{WorkerID: other, Path: "x.apk"}, "", 0), func(json.RawMessage) {}); proto.CodeOf(err) != proto.CodeWorkerOffline {
		t.Fatalf("without a Hub: %v", err)
	}
}
