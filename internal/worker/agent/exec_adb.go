package agent

// Device control (on-device adb) of the Android Worker: the adb_* ops relay to
// the app's bridge through a devctl.Service, which decides the policy (only on
// Android, only when the app reports the feature enabled, only on an unshared
// Worker) and audits every operation. adb_shell, adb_screencap and adb_input
// are served only to a local caller (a session of this Worker through its
// control socket); the Hub can read the status and disable, never enable.

import (
	"context"
	"errors"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/devctl"
)

// AttachDeviceControl registers the adb_* ops served by svc and makes share
// changes go through its guard. It is called once, before the Worker serves
// requests.
func (e *Executor) AttachDeviceControl(svc *devctl.Service) {
	e.devctl.Store(svc)
	e.Handle(ops.OpAdbStatus, e.adbStatus)
	e.Handle(ops.OpAdbShell, e.adbShell)
	e.Handle(ops.OpAdbScreencap, e.adbScreencap)
	e.Handle(ops.OpAdbInput, e.adbInput)
	e.Handle(ops.OpAdbDisable, e.adbDisable)
}

// DeviceControl returns the attached service, or nil.
func (e *Executor) DeviceControl() *devctl.Service { return e.devctl.Load() }

// DeviceShareMode is the share mode the device control policy reads.
func (e *Executor) DeviceShareMode(ctx context.Context) (string, error) {
	st, err := e.policy.State(ctx)
	if err != nil {
		return "", err
	}
	return string(st.Mode), nil
}

// DevicePanes maps the pane pids of the session host to their session names,
// for the session attribution of the audit.
func (e *Executor) DevicePanes(ctx context.Context) (map[int]string, error) {
	sessions, err := e.host.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	m := map[int]string{}
	for _, s := range sessions {
		for _, p := range s.Panes {
			if p.Ref.PID > 0 {
				m[p.Ref.PID] = s.Name
			}
		}
	}
	return m, nil
}

// guardShareForDevice refuses a share change while device control is on.
func (e *Executor) guardShareForDevice(ctx context.Context, mode string) error {
	svc := e.devctl.Load()
	if svc == nil {
		return nil
	}
	if err := svc.GuardShare(ctx, mode); err != nil {
		return apiErr(proto.CodeConflict, "", err.Error())
	}
	return nil
}

// shareChangedForDevice tells device control the committed share mode.
func (e *Executor) shareChangedForDevice(ctx context.Context, mode string) {
	if svc := e.devctl.Load(); svc != nil {
		svc.ShareChanged(context.WithoutCancel(ctx), mode)
	}
}

func (e *Executor) adbService() (*devctl.Service, error) {
	svc := e.devctl.Load()
	if svc == nil {
		return nil, apiErr(proto.CodeUnsupportedOp, "", devctl.Reason(devctl.ErrUnsupportedOS))
	}
	return svc, nil
}

// adbSelf checks a named worker_id against the Worker's own id.
func (e *Executor) adbSelf(ctx context.Context, named string) error {
	if named == "" {
		if hubOrigin(ctx) != nil {
			return apiErr(proto.CodeInvalidArgument, "", "worker_id is required")
		}
		return nil
	}
	if id := e.policy.WorkerID(); id == "" || id != named {
		return apiErr(proto.CodeForbidden, "", "worker_id names another Worker; a Worker only reports or disables its own device control")
	}
	return nil
}

// adbLocalOnly refuses the shell-power ops from the Hub.
func adbLocalOnly(ctx context.Context) error {
	if hubOrigin(ctx) != nil {
		return apiErr(proto.CodeForbidden, "", "device control runs only from a session of this Worker (minion adb); the Hub can only read its status and disable it")
	}
	return nil
}

// adbError maps a devctl failure to the error a caller gets. The text is the
// honest reason; it never holds a command or output.
func adbError(err error) error {
	code := devctl.CodeOf(err)
	switch code {
	case "":
		if errors.Is(err, context.Canceled) {
			return err
		}
		return apiErr(proto.CodeInternal, "", "device control failed")
	case devctl.ErrShared, devctl.ErrDisabled, devctl.ErrNotAvailable, devctl.ErrUnsupportedOS, devctl.ErrBadToken, devctl.ErrEndpointUnverified:
		return apiErr(proto.CodeForbidden, "", err.Error())
	case devctl.ErrBusy:
		return apiErr(proto.CodeBusy, "", err.Error())
	case devctl.ErrBadRequest:
		return apiErr(proto.CodeInvalidArgument, "", err.Error())
	}
	return apiErr(proto.CodeConflict, "", err.Error())
}

func toOpsStatus(r devctl.Report) ops.DeviceControlStatus {
	return ops.DeviceControlStatus{Available: r.Available, Enabled: r.Enabled, State: r.State, Port: r.Port, Transport: r.Transport,
		KeyStorage: r.KeyStorage, ActiveStreams: r.ActiveStreams, LastOK: r.LastOK, LastError: r.LastError,
		Shared: r.Shared, Reason: r.Reason}
}

func (e *Executor) adbStatus(ctx context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.AdbStatusArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := e.adbSelf(ctx, args.WorkerID); err != nil {
		return nil, err
	}
	svc, err := e.adbService()
	if err != nil {
		return nil, err
	}
	// A status read of the Hub (its panel, its sharing guard) is not an
	// operation of a session and is not audited; it would push the
	// operations out of the last 20 entries the panel shows.
	report := svc.Report
	if hubOrigin(ctx) == nil {
		report = svc.Status
	}
	res := ops.AdbStatusResult{DeviceControl: toOpsStatus(report(ctx)), Audit: []ops.AdbAuditEntry{}}
	for _, a := range svc.Recent() {
		res.Audit = append(res.Audit, ops.AdbAuditEntry(a))
	}
	return res, nil
}

func (e *Executor) adbRun(ctx context.Context, op, command string, timeoutS int) (any, error) {
	svc, err := e.adbService()
	if err != nil {
		return nil, err
	}
	code, err := svc.Run(ctx, op, command, time.Duration(timeoutS)*time.Second, func(stderr bool, b []byte) error {
		stream := "stdout"
		if stderr {
			stream = "stderr"
		}
		return EmitProgress(ctx, ops.AdbOutput{Stream: stream, Data: b})
	})
	if err != nil {
		return nil, adbError(err)
	}
	return ops.AdbRunResult{ExitCode: code}, nil
}

func (e *Executor) adbShell(ctx context.Context, rq Request) (any, error) {
	if err := adbLocalOnly(ctx); err != nil {
		return nil, err
	}
	_, args, err := prepare[ops.AdbShellArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := e.adbSelf(ctx, args.WorkerID); err != nil {
		return nil, err
	}
	cmd, err := devctl.ShellCommand(args.Argv)
	if err != nil {
		return nil, apiErr(proto.CodeInvalidArgument, "", err.Error())
	}
	return e.adbRun(ctx, devctl.OpShell, cmd, args.TimeoutS)
}

func (e *Executor) adbScreencap(ctx context.Context, rq Request) (any, error) {
	if err := adbLocalOnly(ctx); err != nil {
		return nil, err
	}
	_, args, err := prepare[ops.AdbScreencapArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := e.adbSelf(ctx, args.WorkerID); err != nil {
		return nil, err
	}
	return e.adbRun(ctx, devctl.OpScreencap, "", args.TimeoutS)
}

func (e *Executor) adbInput(ctx context.Context, rq Request) (any, error) {
	if err := adbLocalOnly(ctx); err != nil {
		return nil, err
	}
	_, args, err := prepare[ops.AdbInputArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := e.adbSelf(ctx, args.WorkerID); err != nil {
		return nil, err
	}
	cmd, err := devctl.InputCommand(append([]string{args.Kind}, args.Args...))
	if err != nil {
		return nil, apiErr(proto.CodeInvalidArgument, "", err.Error())
	}
	return e.adbRun(ctx, devctl.OpInput, cmd, 30)
}

func (e *Executor) adbDisable(ctx context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.AdbDisableArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := e.adbSelf(ctx, args.WorkerID); err != nil {
		return nil, err
	}
	svc, err := e.adbService()
	if err != nil {
		return nil, err
	}
	reason := devctl.ReasonLocal
	if hubOrigin(ctx) != nil {
		reason = devctl.ReasonHub
	}
	if err := svc.Disable(ctx, reason); err != nil {
		return nil, adbError(err)
	}
	return ops.AdbDisableResult{DeviceControl: toOpsStatus(svc.Report(ctx))}, nil
}
