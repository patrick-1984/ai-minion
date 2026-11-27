package server

import (
	"context"
	"errors"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// deviceGuardTimeout bounds the status question to the target Worker.
const deviceGuardTimeout = 5 * time.Second

// errDeviceControlOn is the refusal of a grant to a Worker with device
// control on.
const errDeviceControlOn = "device control (on-device adb) is on for this worker: it needs a dedicated, unshared worker, so access for other sessions is refused; disable device control first"

// deviceStatusCaller is what the guard needs of the router.
type deviceStatusCaller interface {
	Call(ctx context.Context, req workers.CallRequest) (any, error)
}

// deviceControlGuard asks the target Worker for its device control status
// (adb_status, as the caller) before a grant lets other sessions reach it. A
// Worker that does not offer device control (not Android, an older version) or
// is offline is not refused; any other failure to tell is refused (fail
// closed).
func deviceControlGuard(rt deviceStatusCaller) func(ctx context.Context, p authz.Principal, workerID string) error {
	return func(ctx context.Context, p authz.Principal, workerID string) error {
		ctx, cancel := context.WithTimeout(ctx, deviceGuardTimeout)
		defer cancel()
		res, err := rt.Call(ctx, workers.CallRequest{Principal: p, Op: ops.OpAdbStatus, WorkerID: workerID,
			Args: ops.AdbStatusArgs{WorkerID: workerID}})
		if err != nil {
			switch api.ErrorOf(err).Code {
			case proto.CodeUnsupportedOp, proto.CodeWorkerOffline:
				return nil
			}
			var pe *proto.Error
			if errors.As(err, &pe) && (pe.Code == proto.CodeUnsupportedOp || pe.Code == proto.CodeWorkerOffline) {
				return nil
			}
			return api.NewError(proto.CodeConflict, "cannot confirm that device control is off on this worker; try again when it is online and answers")
		}
		var st ops.AdbStatusResult
		switch v := res.(type) {
		case *ops.AdbStatusResult:
			st = *v
		case ops.AdbStatusResult:
			st = v
		default:
			return api.NewError(proto.CodeConflict, "cannot confirm that device control is off on this worker")
		}
		if st.DeviceControl.Enabled {
			return api.NewError(proto.CodeConflict, errDeviceControlOn)
		}
		return nil
	}
}
