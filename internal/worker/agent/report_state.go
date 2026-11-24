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

// harnessTitleReader supplies the name the user gave the harness session
// (store.Store implements it; a reader without it reports no name).
type harnessTitleReader interface {
	HarnessTitle(ctx context.Context, session, pane, harness string, created int64, now time.Time) (string, int, error)
}

type harnessLocator interface {
	Locate(context.Context, agentdetect.Input) (agentdetect.Result, int)
}

const nativeStateTimeout = 100 * time.Millisecond

// reportState supplies display metadata to shared reads and event snapshots.
// The classifier result used by input guards and terminal readiness stays local.
// It returns the pid of the pane's harness when it located it (0 otherwise).
func reportState(ctx context.Context, reader nativeStateReader, classifier Classifier, info *ops.PaneInfo, screen ops.Screen, created int64, classifiedBackground bool) int {
	harness, isAgent := strings.CutPrefix(info.Kind, ops.PaneKindAgentPrefix)
	if !isAgent {
		return 0
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
				return pid
			}
		}
		cancel()
	}
	if background {
		info.State = ops.PaneStateBusy
		info.StateReason = ops.PaneStateReasonBackground
	}
	return 0
}

// reportUnreadablePane lets inventory use a verified native source even when
// tmux could not capture a screen. Without that proof the pane stays unknown.
// It returns the pid of the pane's harness when it used its native state.
func reportUnreadablePane(ctx context.Context, reader nativeStateReader, classifier Classifier, info *ops.PaneInfo, created int64) int {
	locator, ok := classifier.(harnessLocator)
	if !ok || reader == nil || created <= 0 {
		return 0
	}
	readCtx, cancel := context.WithTimeout(ctx, nativeStateTimeout)
	defer cancel()
	result, pid := locator.Locate(readCtx, agentdetect.Input{Command: info.Command, PID: info.PID})
	harness, isAgent := strings.CutPrefix(result.Kind, ops.PaneKindAgentPrefix)
	if !isAgent || pid <= 0 {
		return 0
	}
	native, found, err := reader.NativeHarnessState(readCtx, info.Session, info.ID, harness, created, time.Now())
	if err != nil || !found || pid != native.ProcessPID || readCtx.Err() != nil {
		return 0
	}
	info.Kind, info.InputKind = result.Kind, result.Kind
	applyNativeState(info, native)
	return pid
}

// reportedHarnessName returns the name the user gave the harness session of a
// pane whose state reportState or reportUnreadablePane filled in info, also
// when the state itself comes from the screen ("" for none). The name belongs
// to the process that last reported in this pane generation; pid is that of
// the pane's harness when the caller already located it (0: locate it here).
// Only the Worker's state stream to its Hub carries it (the Hub binds it to
// the owner the connection was admitted under); operation results never do.
func reportedHarnessName(ctx context.Context, reader nativeStateReader, classifier Classifier, info ops.PaneInfo, created int64, pid int) string {
	harness, isAgent := strings.CutPrefix(info.Kind, ops.PaneKindAgentPrefix)
	titles, readable := reader.(harnessTitleReader)
	locator, canLocate := classifier.(harnessLocator)
	if !isAgent || !readable || !canLocate || created <= 0 || info.InputKind != info.Kind {
		return ""
	}
	readCtx, cancel := context.WithTimeout(ctx, nativeStateTimeout)
	defer cancel()
	title, owner, err := titles.HarnessTitle(readCtx, info.Session, info.ID, harness, created, time.Now())
	if err != nil || title == "" || owner <= 0 {
		return ""
	}
	if pid == 0 {
		var result agentdetect.Result
		result, pid = locator.Locate(readCtx, agentdetect.Input{Command: info.Command, PID: info.PID})
		if result.Kind != info.Kind {
			return ""
		}
	}
	if pid != owner || readCtx.Err() != nil {
		return ""
	}
	return ops.SanitizeHarnessName(title)
}

func applyNativeState(info *ops.PaneInfo, native store.HarnessState) {
	info.State, info.StateSource, info.StateReason = native.State, native.Source, native.Reason
	info.LastEvent = ops.Timestamp(native.LastEvent.UTC())
}
