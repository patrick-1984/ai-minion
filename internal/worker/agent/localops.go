package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// Local-only management ops (tray tickets v3, W0).
//
// These ops manage the Worker itself: its status, its lifecycle, its Hub trust
// and (with the tray tickets) pause, configuration and sharing. They are
// served by the local control socket only, whose peer is the OS user that
// owns the Worker, and are dispatched in [localExecutor] before anything else:
// before a session-control request is forwarded to the Hub and before the
// [Executor]. They are never handlers of the Executor, so the Hub cannot call
// them (its op stream reaches the Executor only, which answers
// unsupported_op), and they are never announced in the hello (Executor.Ops).

// localOp serves one local-only op.
type localOp func(l localExecutor, ctx context.Context, rq localapi.Request) (json.RawMessage, error)

// localOps is the dispatch map of the local-only ops. Its keys are checked
// against an independent literal list in localonly_e2e_test.go.
var localOps = map[string]localOp{
	OpLocalStatus:    localStatusOp,
	OpLocalStop:      localStopOp,
	OpLocalVerifyHub: localVerifyHubOp,
	OpLocalPause: func(l localExecutor, ctx context.Context, rq localapi.Request) (json.RawMessage, error) {
		return l.pauseOp(ctx, rq, true)
	},
	OpLocalResume: func(l localExecutor, ctx context.Context, rq localapi.Request) (json.RawMessage, error) {
		return l.pauseOp(ctx, rq, false)
	},
	OpLocalConfigApply: func(l localExecutor, ctx context.Context, rq localapi.Request) (json.RawMessage, error) {
		return l.configApplyOp(ctx, rq)
	},
	OpLocalShareList:  localShareListOp,
	OpLocalShareApply: localShareApplyOp,
}

// localFeatures are the capabilities of the local-only ops (for example
// lifecycle.v1) that a capability-qualified request may require. They are
// added only to the qualified-op check of the local socket, never to
// [Executor.Features], so they are not announced to the Hub.
var localFeatures = []string{FeaturePause, FeatureConfigApply, FeatureShareLocal}

// localOpFor returns the local-only handler of op, if there is one.
func localOpFor(op string) (localOp, bool) {
	h, ok := localOps[op]
	return h, ok
}

func localStatusOp(l localExecutor, ctx context.Context, _ localapi.Request) (json.RawMessage, error) {
	if l.live == nil {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "this Worker has no live status source")
	}
	return statusOp(ctx, l.stateDir, l.instance, l.live)
}

func localStopOp(l localExecutor, _ context.Context, _ localapi.Request) (json.RawMessage, error) {
	if l.stop == nil {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "this Worker cannot be stopped over its control socket")
	}
	// Reply first: the stop closes this socket.
	go func() { time.Sleep(200 * time.Millisecond); l.stop() }()
	return json.RawMessage(`{"stopping":true}`), nil
}

func localVerifyHubOp(l localExecutor, ctx context.Context, rq localapi.Request) (json.RawMessage, error) {
	return l.verifyHub(ctx, rq.Args)
}
