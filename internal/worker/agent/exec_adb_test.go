package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/devctl"
	"github.com/patrick-1984/ai-minion/internal/worker/devctl/devctltest"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

const adbToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// adbEnv is an executor with device control attached to a fake bridge, as on
// an unshared Android Worker.
func adbEnv(t *testing.T) (*env, *devctltest.Bridge) {
	t.Helper()
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareNone}, "build")
	dir, err := os.MkdirTemp("", "dca")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	fake, err := devctltest.Start(dir, adbToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	svc := devctl.New(devctl.Config{GOOS: "android", Bridge: devctl.NewClient(fake.Path, func() string { return adbToken }),
		ShareMode: e.ex.DeviceShareMode, Panes: e.ex.DevicePanes})
	e.ex.AttachDeviceControl(svc)
	return e, fake
}

func TestAdbShellStreamsOutputLocally(t *testing.T) {
	e, fake := adbEnv(t)
	fake.Handle(func(op, cmd string) devctltest.Result {
		if op == devctl.OpShell && cmd == "sh -c 'exit 7'" {
			return devctltest.Result{Stdout: []byte("out"), Code: 7}
		}
		return devctltest.Result{Code: 1}
	})
	var got []ops.AdbOutput
	ctx := WithProgress(context.Background(), func(b json.RawMessage) {
		var o ops.AdbOutput
		_ = json.Unmarshal(b, &o)
		got = append(got, o)
	})
	raw, err := e.ex.Execute(ctx, Request{Op: ops.OpAdbShell, Actions: allActions,
		Args: mustJSON(t, ops.AdbShellArgs{Argv: []string{"sh", "-c", "exit 7"}})})
	if err != nil {
		t.Fatal(err)
	}
	var res ops.AdbRunResult
	_ = json.Unmarshal(raw, &res)
	if res.ExitCode != 7 || len(got) != 1 || string(got[0].Data) != "out" || got[0].Stream != "stdout" {
		t.Fatalf("res %+v out %+v", res, got)
	}
}

func TestAdbShellRefusedFromTheHub(t *testing.T) {
	e, fake := adbEnv(t)
	for _, op := range []string{ops.OpAdbShell, ops.OpAdbScreencap, ops.OpAdbInput} {
		args := map[string]any{"worker_id": shareWorkerID, "argv": []string{"id"}, "kind": "keyevent", "args": []string{"KEYCODE_HOME"}}
		_, err := e.ex.Execute(context.Background(), Request{Op: op, Actions: allActions, Args: mustJSON(t, args),
			Origin: &proto.Origin{}})
		if ae, ok := AsAPIError(err); !ok || ae.Code != proto.CodeForbidden {
			t.Fatalf("%s from the Hub: %v", op, err)
		}
	}
	for _, op := range fake.Ops() {
		if op != devctl.OpStatus {
			t.Fatalf("the bridge got %s", op)
		}
	}
}

func TestAdbStatusAndDisableFromTheHub(t *testing.T) {
	e, fake := adbEnv(t)
	hub := &proto.Origin{}
	raw, err := e.ex.Execute(context.Background(), Request{Op: ops.OpAdbStatus, Actions: allActions,
		Args: mustJSON(t, ops.AdbStatusArgs{WorkerID: shareWorkerID}), Origin: hub})
	if err != nil {
		t.Fatal(err)
	}
	var st ops.AdbStatusResult
	_ = json.Unmarshal(raw, &st)
	if !st.DeviceControl.Available || !st.DeviceControl.Enabled || st.DeviceControl.State != "READY" || len(st.Audit) != 1 {
		t.Fatalf("status %+v", st)
	}
	// Another Worker's id is refused.
	if _, err := e.ex.Execute(context.Background(), Request{Op: ops.OpAdbDisable, Actions: allActions,
		Args: mustJSON(t, ops.AdbDisableArgs{WorkerID: "01900000-0000-7000-8000-000000000002"}), Origin: hub}); err == nil {
		t.Fatal("a foreign worker_id was accepted")
	}
	raw, err = e.ex.Execute(context.Background(), Request{Op: ops.OpAdbDisable, Actions: allActions,
		Args: mustJSON(t, ops.AdbDisableArgs{WorkerID: shareWorkerID}), Origin: hub})
	if err != nil {
		t.Fatal(err)
	}
	var dr ops.AdbDisableResult
	_ = json.Unmarshal(raw, &dr)
	if dr.DeviceControl.Enabled || fake.Status().Enabled {
		t.Fatalf("still enabled: %+v", dr)
	}
}

func TestAdbInputValidatedBeforeSending(t *testing.T) {
	e, fake := adbEnv(t)
	_, err := e.call(ops.OpAdbInput, allActions, ops.AdbInputArgs{Kind: "keyevent", Args: []string{"HOME; reboot"}})
	if ae, ok := AsAPIError(err); !ok || ae.Code != proto.CodeInvalidArgument {
		t.Fatalf("err %v", err)
	}
	if len(fake.Ops()) != 0 {
		t.Fatalf("the bridge got %v", fake.Ops())
	}
	if _, err := e.call(ops.OpAdbInput, allActions, ops.AdbInputArgs{Kind: "keyevent", Args: []string{"KEYCODE_HOME"}}); err != nil {
		t.Fatal(err)
	}
	rq := fake.Requests()
	if last := rq[len(rq)-1]; last.Op != devctl.OpInput || last.Command != "input keyevent KEYCODE_HOME" {
		t.Fatalf("request %+v", last)
	}
}

// Sharing is refused while device control is on; once it is off, sharing
// works again.
func TestShareSetRefusedWhileDeviceControlIsOn(t *testing.T) {
	e, fake := adbEnv(t)
	_, err := e.share(ops.OpShareSet, operateActions, shareSetArgs("all"))
	if ae, ok := AsAPIError(err); !ok || ae.Code != proto.CodeConflict || !strings.Contains(ae.Message, "device control") {
		t.Fatalf("share_set all: %v", err)
	}
	if got := e.saved(); got.Mode != store.ShareNone {
		t.Fatalf("saved %+v", got)
	}
	if _, err := e.call(ops.OpAdbDisable, allActions, ops.AdbDisableArgs{}); err != nil {
		t.Fatal(err)
	}
	e.mustShare(ops.OpShareSet, shareSetArgs("all"))
	if fake.Status().Enabled {
		t.Fatal("enabled")
	}
}

// A share change that does not go through the guard (the bridge was down)
// still disables device control afterwards, and calls are refused.
func TestShareChangeBehindTheGuardDisables(t *testing.T) {
	e, fake := adbEnv(t)
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareAll}); err != nil {
		t.Fatal(err)
	}
	_, err := e.call(ops.OpAdbShell, allActions, ops.AdbShellArgs{Argv: []string{"id"}})
	if ae, ok := AsAPIError(err); !ok || ae.Code != proto.CodeForbidden || !strings.Contains(ae.Message, "shared") {
		t.Fatalf("err %v", err)
	}
	if fake.Status().Enabled {
		t.Fatal("a shared Worker kept device control enabled")
	}
}

func TestDeviceControlInLocalStatus(t *testing.T) {
	d := ops.DeviceControlStatus{Available: true, Enabled: true, State: "READY", Port: 37000, KeyStorage: "android-keystore (TEE)"}
	r := NewLiveStatusReply("/s", LiveStatus{DeviceControl: &d})
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"device_control":{"available":true,"enabled":true,"state":"READY","port":37000`) {
		t.Fatalf("%s", b)
	}
	var back LiveStatusReply
	_ = json.Unmarshal(b, &back)
	live, err := back.LiveStatus()
	if err != nil || live.DeviceControl == nil || live.DeviceControl.Port != 37000 {
		t.Fatalf("%+v %v", live.DeviceControl, err)
	}
	if got := deviceControlText(d); got != "on (TLS loopback, port 37000, key android-keystore (TEE))" {
		t.Fatal(got)
	}
	if b, _ := json.Marshal(NewLiveStatusReply("/s", LiveStatus{})); strings.Contains(string(b), "device_control") {
		t.Fatal("a Worker without device control reports the block")
	}
}
