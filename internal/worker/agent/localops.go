package agent

import (
	"context"
	"encoding/json"
	"fmt"
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
	OpLocalLifecycle:  localLifecycleOp,
	OpLocalPairCancel: func(l localExecutor, ctx context.Context, rq localapi.Request) (json.RawMessage, error) {
		return l.pairCancelOp(ctx, rq)
	},
}

// localFeatures are the capabilities of the local-only ops (for example
// lifecycle.v1) that a capability-qualified request may require. They are
// added only to the qualified-op check of the local socket, never to
// [Executor.Features], so they are not announced to the Hub.
var localFeatures = []string{FeaturePause, FeatureConfigApply, FeatureShareLocal, FeatureShareReplace, FeatureLifecycle, FeatureStopGuard, FeaturePairingStatus, FeaturePairCancel}

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

// checkStopGuard refuses (conflict, state_changed) a stop whose caller read
// another instance or another enrollment generation than this Worker's.
func checkStopGuard(ctx context.Context, instance, stateDir string, want LocalStopArgs) error {
	if want.Instance != "" && want.Instance != instance {
		return apiErr(proto.CodeConflict, "state_changed", "the Worker restarted since its status was read; nothing was done")
	}
	if want.EnrollmentEpoch != nil {
		epoch, err := ReadEnrollmentEpoch(ctx, stateDir)
		if err != nil {
			return internalErr("read the enrollment generation", err)
		}
		if epoch != *want.EnrollmentEpoch {
			return apiErr(proto.CodeConflict, "state_changed", fmt.Sprintf("the enrollment changed (enrollment_epoch is %d, expected %d); nothing was done", epoch, *want.EnrollmentEpoch))
		}
	}
	return nil
}

func localStopOp(l localExecutor, ctx context.Context, rq localapi.Request) (json.RawMessage, error) {
	if l.stop == nil {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "this Worker cannot be stopped over its control socket")
	}
	var a LocalStopArgs
	if len(rq.Args) > 0 && string(rq.Args) != "null" {
		if err := json.Unmarshal(rq.Args, &a); err != nil {
			return nil, apiErr(proto.CodeInvalidArgument, "", "invalid stop request")
		}
	}
	if err := checkStopGuard(ctx, l.instance, l.stateDir, a); err != nil {
		return nil, err
	}
	// Reply first: the stop closes this socket.
	go func() { time.Sleep(200 * time.Millisecond); l.stop() }()
	return json.RawMessage(`{"stopping":true}`), nil
}

func localVerifyHubOp(l localExecutor, ctx context.Context, rq localapi.Request) (json.RawMessage, error) {
	return l.verifyHub(ctx, rq.Args)
}
