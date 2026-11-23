package agent

import (
	"context"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

type nativeStateReader interface {
	NativeHarnessState(ctx context.Context, session, pane, harness string, created int64, now time.Time) (store.HarnessState, bool, error)
}

type harnessLocator interface {
	Locate(context.Context, agentdetect.Input) (agentdetect.Result, int)
}

const nativeStateTimeout = 100 * time.Millisecond

// reportState supplies display metadata to shared reads and event snapshots.
// The classifier result used by input guards and terminal readiness stays local.
func reportState(ctx context.Context, reader nativeStateReader, classifier Classifier, info *ops.PaneInfo, screen ops.Screen, created int64, classifiedBackground bool) {
	harness, isAgent := strings.CutPrefix(info.Kind, ops.PaneKindAgentPrefix)
	if !isAgent {
		return
	}
	info.StateSource = ops.PaneStateSourceScreen
	background := classifiedBackground || (harness == "claude" && (info.State == ops.PaneStateStable || info.State == ops.PaneStateUnknown) &&
		agentdetect.ClaudeBackgroundWork(screen))
	locator, canLocate := classifier.(harnessLocator)
	if reader != nil && canLocate && created > 0 && info.InputKind == info.Kind {
		readCtx, cancel := context.WithTimeout(ctx, nativeStateTimeout)
		native, ok, err := reader.NativeHarnessState(readCtx, info.Session, info.ID, harness, created, time.Now())
		// A visible input dialog keeps its reviewed binding with the screen.
		// Matching native evidence can supply the source and time.
		if err == nil && ok && native.ProcessPID > 0 &&
			(info.State != ops.PaneStateDialog || native.State == ops.PaneStateDialog) &&
			(!background || native.RuntimeKnown || native.State != ops.PaneStateStable) {
			result, pid := locator.Locate(readCtx, agentdetect.Input{Command: info.Command, PID: info.PID})
			if pid == native.ProcessPID && result.Kind == info.InputKind && readCtx.Err() == nil {
				applyNativeState(info, native)
				cancel()
				return
			}
		}
		cancel()
	}
	if background {
		info.State = ops.PaneStateBusy
		info.StateReason = ops.PaneStateReasonBackground
	}
}

// reportUnreadablePane lets inventory use a verified native source even when
// tmux could not capture a screen. Without that proof the pane stays unknown.
func reportUnreadablePane(ctx context.Context, reader nativeStateReader, classifier Classifier, info *ops.PaneInfo, created int64) {
	locator, ok := classifier.(harnessLocator)
	if !ok || reader == nil || created <= 0 {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, nativeStateTimeout)
	defer cancel()
	result, pid := locator.Locate(readCtx, agentdetect.Input{Command: info.Command, PID: info.PID})
	harness, isAgent := strings.CutPrefix(result.Kind, ops.PaneKindAgentPrefix)
	if !isAgent || pid <= 0 {
		return
	}
	native, found, err := reader.NativeHarnessState(readCtx, info.Session, info.ID, harness, created, time.Now())
	if err != nil || !found || pid != native.ProcessPID || readCtx.Err() != nil {
		return
	}
	info.Kind, info.InputKind = result.Kind, result.Kind
	applyNativeState(info, native)
}

func applyNativeState(info *ops.PaneInfo, native store.HarnessState) {
	info.State, info.StateSource, info.StateReason = native.State, native.Source, native.Reason
	info.LastEvent = ops.Timestamp(native.LastEvent.UTC())
}
