package server

import (
	"context"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type fakeStatusCaller struct {
	res any
	err error
	req workers.CallRequest
}

func (f *fakeStatusCaller) Call(_ context.Context, req workers.CallRequest) (any, error) {
	f.req = req
	return f.res, f.err
}

// A grant to a Worker with device control on is refused; off, not Android
// (unsupported_op) or offline is allowed; any other failure is refused.
func TestDeviceControlGuard(t *testing.T) {
	p := authz.NewSessionPrincipal("u1")
	cases := []struct {
		name   string
		res    any
		err    error
		refuse string
	}{
		{"on", ops.AdbStatusResult{DeviceControl: ops.DeviceControlStatus{Enabled: true}}, nil, "device control (on-device adb) is on"},
		{"off", ops.AdbStatusResult{DeviceControl: ops.DeviceControlStatus{Available: true}}, nil, ""},
		{"not android", nil, proto.NewError(proto.CodeUnsupportedOp, "no"), ""},
		{"offline", nil, api.NewError(proto.CodeWorkerOffline, "offline"), ""},
		{"unknown", nil, proto.NewError(proto.CodeInternal, "boom"), "cannot confirm"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeStatusCaller{res: c.res, err: c.err}
			err := deviceControlGuard(f)(context.Background(), p, "w-target")
			if f.req.Op != ops.OpAdbStatus || f.req.WorkerID != "w-target" || f.req.Principal.UserID != "u1" {
				t.Fatalf("request %+v", f.req)
			}
			switch {
			case c.refuse == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.refuse != "" && (err == nil || !strings.Contains(err.Error(), c.refuse) || api.ErrorOf(err).Code != proto.CodeConflict):
				t.Fatalf("err %v", err)
			}
		})
	}
}
