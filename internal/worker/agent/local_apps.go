package agent

// The local daemon as the caller of an app install on another Worker
// (v0.25.0, docs/guides/android-apps.md): an app_install or
// app_install_status on the local socket whose worker_id names another
// Worker. The Hub decides and audits every install; the authority of a
// session caller is the apps_install flag of a verified grant or master
// acceptance, so the request goes on the master lane when the calling
// session holds a verified lease and the caller runs below its harness
// (masterApps, master_files_linux.go). Otherwise it goes on an ordinary call
// stream with the calling session as source_session (a claimed source),
// which the Hub refuses with a message that says how to get a verified one.

import (
	"context"
	"encoding/json"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// forwardsApp reports whether a local app_install or app_install_status
// names another Worker than self.
func forwardsApp(op string, raw json.RawMessage, self string) bool {
	if op != ops.OpAppInstall && op != ops.OpAppInstallStatus {
		return false
	}
	var a struct {
		WorkerID string `json:"worker_id"`
	}
	_ = json.Unmarshal(raw, &a)
	return a.WorkerID != "" && (self == "" || a.WorkerID != self)
}

// decodeAppArgs reads and validates the arguments of a forwarded app op.
func decodeAppArgs(op string, raw json.RawMessage) (any, error) {
	bad := apiErr(proto.CodeInvalidArgument, "", "the arguments of "+op+" cannot be read")
	if op == ops.OpAppInstall {
		var a ops.AppInstallArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, bad
		}
		if err := a.Validate(); err != nil {
			return nil, apiErr(proto.CodeInvalidArgument, "", err.Error())
		}
		return a, nil
	}
	var a ops.AppInstallStatusArgs
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, bad
		}
	}
	if err := a.Validate(); err != nil {
		return nil, apiErr(proto.CodeInvalidArgument, "", err.Error())
	}
	return a, nil
}

// appCall sends a local app op of another Worker to the Hub. The local
// socket's actions do not decide it: the Hub does.
func (l localExecutor) appCall(ctx context.Context, rq Request, caller callerPane) (json.RawMessage, error) {
	args, err := decodeAppArgs(rq.Op, rq.Args)
	if err != nil {
		return nil, err
	}
	var cc PipeCaller
	if l.calls != nil {
		cc = l.calls()
	}
	if c, ok := cc.(interface{ Connected() bool }); cc == nil || (ok && !c.Connected()) {
		return nil, apiErr(proto.CodeWorkerOffline, "", "the request names another Worker, which only the Hub can reach, and this Worker is not connected to a Hub")
	}
	session := l.ex.sourceSession(ctx, caller)
	if session != "" && caller.PeerPID > 0 && l.masterApps != nil {
		if ma := l.masterApps(); ma != nil {
			var res json.RawMessage
			if handled, err := ma(ctx, session, caller.PeerPID, rq.Op, args, &res); handled {
				if err != nil {
					return nil, forwardError(err)
				}
				return res, nil
			}
		}
	}
	var res json.RawMessage
	if err := cc.Do(WithSourceSession(ctx, session), rq.Op, args, &res); err != nil {
		return nil, forwardError(err)
	}
	if len(res) == 0 {
		res = json.RawMessage(`null`)
	}
	return res, nil
}
