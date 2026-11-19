package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// The capability-qualified lifecycle op (tray W2b): stop and restart with a
// session-loss preflight. A client sends it as worker_lifecycle+lifecycle.v1,
// so an older Worker refuses it as unsupported instead of running the bare
// worker_stop, which ignores its arguments and could end ConPTY sessions
// without a count.
const (
	OpLocalLifecycle = "worker_lifecycle"
	// FeatureLifecycle is its local capability.
	FeatureLifecycle = "lifecycle.v1"
)

// Lifecycle actions.
const (
	LifecycleStop    = "stop"
	LifecycleRestart = "restart"
)

// Reasons of the lifecycle refusals (details reason of a conflict).
const (
	ReasonSessionsWouldEnd = "sessions_would_end"
	ReasonCountUnavailable = "count_unavailable"
)

// lifecycleHoldTimeout ends the create hold of a stop that the caller
// completes through the service manager, should that never happen.
const lifecycleHoldTimeout = 2 * time.Minute

// LocalLifecycleArgs are the arguments of [OpLocalLifecycle].
type LocalLifecycleArgs struct {
	Action   string `json:"action"`
	Instance string `json:"instance"`
	// AcceptLoss is the number of sessions the caller accepts to end; a
	// larger count refuses again.
	AcceptLoss int `json:"accept_loss"`
	// AcceptUnknown accepts that sessions that cannot be counted end.
	AcceptUnknown bool `json:"accept_unknown"`
}

// LifecycleResult is the reply of [OpLocalLifecycle].
type LifecycleResult struct {
	Action string `json:"action"`
	// SessionsLost is how many sessions end; -1 when they could not be
	// counted and the caller accepted that.
	SessionsLost int `json:"sessions_lost"`
	// Exiting is true when the Worker ends (or restarts) itself; false when
	// the caller stops it through its service manager (systemd, launchd),
	// which would start a Worker that simply exits again. New sessions stay
	// held until then.
	Exiting bool `json:"exiting"`
}

// lifecycleService serves the lifecycle op of the running Worker.
type lifecycleService struct {
	ex       *Executor
	counter  SessionCounter
	runMode  string
	instance string
	// stop and restart end the process (after the reply).
	stop    func()
	restart func()
	// markClean records an intentional restart as a clean stop for the update
	// crash guard (the exit or exec below bypasses the runtime's own stop); nil
	// does nothing.
	markClean func()
}

func (s lifecycleService) serve(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var a LocalLifecycleArgs
	if err := json.Unmarshal(raw, &a); err != nil || (a.Action != LifecycleStop && a.Action != LifecycleRestart) {
		return nil, apiErr(proto.CodeInvalidArgument, "", "invalid lifecycle request: action stop or restart")
	}
	if a.Instance != "" && a.Instance != s.instance {
		return nil, apiErr(proto.CodeConflict, "state_changed", "the Worker restarted since its status was read; nothing was done")
	}
	// No new session from here on, then count the ones that would end.
	release := s.ex.HoldCreate(HoldOwnerLifecycle, "the Worker is stopping")
	// A count gets its own deadline: a hung host must not hold the hold.
	cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	n, known, err := s.counter.CountLost(cctx)
	cancel()
	switch {
	case !known && !a.AcceptUnknown:
		release()
		msg := "the sessions that would end cannot be counted"
		if err != nil {
			msg += " (" + err.Error() + ")"
		}
		return nil, &APIError{Code: proto.CodeConflict, Reason: ReasonCountUnavailable, Message: msg + "; nothing was done"}
	case known && n > a.AcceptLoss:
		release()
		det, _ := json.Marshal(map[string]any{"reason": ReasonSessionsWouldEnd, "sessions_lost": n})
		return nil, &APIError{Code: proto.CodeConflict, Reason: ReasonSessionsWouldEnd, Details: det,
			Message: fmt.Sprintf("%d sessions would end; nothing was done", n)}
	}
	res := LifecycleResult{Action: a.Action, SessionsLost: n}
	if !known {
		res.SessionsLost = -1
	}
	// The hold stays until the process ends. A service manager stop by the
	// caller ends it; should that never come, the hold ends after a while.
	switch {
	case a.Action == LifecycleRestart:
		res.Exiting = true
		go func() {
			time.Sleep(200 * time.Millisecond)
			if s.markClean != nil {
				s.markClean()
			}
			s.restart()
		}()
	case s.runMode == RunModeSystemd || s.runMode == RunModeLaunchd:
		// systemd Restart=always and launchd KeepAlive would start a Worker
		// that exits: the caller stops the unit or agent.
		time.AfterFunc(lifecycleHoldTimeout, release)
	default:
		res.Exiting = true
		go func() { time.Sleep(200 * time.Millisecond); s.stop() }()
	}
	return json.Marshal(res)
}

func localLifecycleOp(l localExecutor, ctx context.Context, rq localapi.Request) (json.RawMessage, error) {
	if l.lifecycle == nil {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "this Worker cannot stop or restart over its control socket")
	}
	return l.lifecycle.serve(ctx, rq.Args)
}

// lifecycleExe is the binary a restart runs: this one.
func lifecycleExe() string {
	exe, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		return real
	}
	return exe
}

// newSessionCounter returns the counter of the Worker's own host.
func newSessionCounter(ex *Executor, hostKind string) SessionCounter {
	return SessionCounter{Host: ex.host, EndsSessions: restartEndsSessions(runtime.GOOS, hostKind)}
}
