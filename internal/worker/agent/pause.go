package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// ReasonPaused is the Status.Reason of an idle connector whose Worker is
// paused (minion worker pause): it does not connect until resumed.
const ReasonPaused = "paused (minion worker resume)"

// CodePaused is the error code of a local request that needs the Hub while
// the Worker is paused.
const CodePaused proto.Code = "paused"

// PauseDrainTimeout bounds the wait of a pause for the writes in flight.
// After it the pause is acknowledged with writes_pending, admission stays
// closed and the writes finish into the result cache.
const PauseDrainTimeout = 30 * time.Second

// PauseGate is the pause switch the connector reads (tray W3b). Every change
// closes the channel Changed returned before it, so that the connector's
// backoff, idle and connection waits wake up at once.
type PauseGate struct {
	mu      sync.Mutex
	paused  bool
	changed chan struct{}
}

// NewPauseGate returns a gate in the given state.
func NewPauseGate(paused bool) *PauseGate {
	return &PauseGate{paused: paused, changed: make(chan struct{})}
}

// Paused reports whether Hub traffic is paused, and the channel that is
// closed at the next change. A nil gate is never paused.
func (g *PauseGate) Paused() (bool, <-chan struct{}) {
	if g == nil {
		return false, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused, g.changed
}

// Set changes the state and wakes the waiters when it changed.
func (g *PauseGate) Set(paused bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused == paused {
		return
	}
	g.paused = paused
	close(g.changed)
	g.changed = make(chan struct{})
}

// pauseAdmission is the op server as the pause uses it.
type pauseAdmission interface {
	CloseAdmission()
	Suspend(ctx context.Context) (pending int, err error)
	Reopen()
}

// PauseService applies pause and resume in the running Worker: the pause
// file, the connector gate, the op admission and the timer of a timed pause.
type PauseService struct {
	dir  string
	log  *slog.Logger
	gate *PauseGate
	// now and afterFunc are the clock; tests replace them.
	now       func() time.Time
	afterFunc func(d time.Duration, f func()) (stop func() bool)
	// set persists a state (SetPause); tests inject failures.
	set   func(dir string, paused bool, until, now time.Time) (PauseState, error)
	drain time.Duration
	// connected reports whether the connector still holds a link.
	connected func() bool

	mu        sync.Mutex
	state     PauseState
	stopTimer func() bool
	admission pauseAdmission
}

// NewPauseService reads the pause file of dir and returns the service with
// its gate in the saved state. A timed pause that ended before the start
// counts as resumed; a pause that runs on is ended by a timer.
func NewPauseService(dir string, log *slog.Logger) (*PauseService, error) {
	return newPauseService(dir, log, time.Now, func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop })
}

func newPauseService(dir string, log *slog.Logger, now func() time.Time, afterFunc func(time.Duration, func()) func() bool) (*PauseService, error) {
	st, err := ReadPause(dir)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &PauseService{dir: dir, log: log, now: now, afterFunc: afterFunc, set: SetPause, drain: PauseDrainTimeout, state: st}
	active := st.Active(now())
	p.gate = NewPauseGate(active)
	if active {
		log.Info("the Worker starts paused: no Hub traffic until minion worker resume", "until", untilText(st.Until))
		p.armTimer(st)
	}
	return p, nil
}

func untilText(t time.Time) string {
	if t.IsZero() {
		return "resumed"
	}
	return t.UTC().Format(time.RFC3339)
}

// Gate returns the switch the connector reads.
func (p *PauseService) Gate() *PauseGate { return p.gate }

// Paused reports whether the Worker is paused now.
func (p *PauseService) Paused() bool {
	paused, _ := p.gate.Paused()
	return paused
}

// attach connects the op server; a paused Worker starts with admission
// closed.
func (p *PauseService) attach(a pauseAdmission) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.admission = a
	if p.Paused() {
		a.CloseAdmission()
	}
}

// armTimer ends a timed pause at its end, if the generation still matches.
// The caller holds p.mu or owns p exclusively.
func (p *PauseService) armTimer(st PauseState) {
	if p.stopTimer != nil {
		p.stopTimer()
		p.stopTimer = nil
	}
	if !st.Paused || st.Until.IsZero() {
		return
	}
	gen := st.Generation
	p.stopTimer = p.afterFunc(st.Until.Sub(p.now()), func() {
		if _, err := p.resume(context.Background(), gen); err != nil {
			p.log.Error("could not end the timed pause", "error", err)
		}
	})
}

// PauseResult is the reply of a pause: the new state and the writes that
// were still running when the drain timeout ended.
type PauseResult struct {
	PauseState
	WritesPending int `json:"writes_pending,omitempty"`
}

// Pause pauses the Hub traffic until until (zero: until resumed). The order
// of the acknowledgement: the state is saved (a failure changes nothing),
// admission is closed, the link is cancelled, the writes in flight are
// drained (at most the drain timeout), then it returns.
func (p *PauseService) Pause(ctx context.Context, until time.Time) (PauseResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if !until.IsZero() && !until.After(now) {
		return PauseResult{}, apiErr(proto.CodeInvalidArgument, "", "the end of the pause is in the past")
	}
	st, err := p.set(p.dir, true, until, now)
	if err != nil {
		return PauseResult{}, internalErr("save the pause", err)
	}
	p.state = st
	if p.admission != nil {
		p.admission.CloseAdmission()
	}
	p.gate.Set(true)
	p.armTimer(st)
	p.waitDisconnected(ctx)
	res := PauseResult{PauseState: st}
	if p.admission != nil {
		dctx, cancel := context.WithTimeout(ctx, p.drain)
		res.WritesPending, _ = p.admission.Suspend(dctx)
		cancel()
	}
	p.log.Info("paused: no Hub traffic", "until", untilText(st.Until), "generation", st.Generation, "writes_pending", res.WritesPending)
	return res, nil
}

// waitDisconnected waits a short time for the connector to drop its link.
func (p *PauseService) waitDisconnected(ctx context.Context) {
	if p.connected == nil {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for p.connected() && time.Now().Before(deadline) && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
}

// Resume ends a pause.
func (p *PauseService) Resume(ctx context.Context) (PauseState, error) {
	return p.resume(ctx, 0)
}

// resume ends the pause; a timer passes its generation (0: any).
func (p *PauseService) resume(_ context.Context, gen uint64) (PauseState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gen != 0 && p.state.Generation != gen {
		return p.state, nil // a later change replaced the timed pause
	}
	st, err := p.set(p.dir, false, time.Time{}, p.now())
	if err != nil {
		return PauseState{}, internalErr("save the resume", err)
	}
	p.state = st
	p.armTimer(st)
	if p.admission != nil {
		p.admission.Reopen()
	}
	p.gate.Set(false)
	p.log.Info("resumed: the Worker connects to its Hub again", "generation", st.Generation)
	return st, nil
}

// pauseOp serves worker_pause and worker_resume on the local socket.
func (l localExecutor) pauseOp(ctx context.Context, rq localapi.Request, pause bool) (json.RawMessage, error) {
	if l.pause == nil {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "this Worker cannot pause")
	}
	var args LocalPauseArgs
	if len(rq.Args) > 0 {
		if err := json.Unmarshal(rq.Args, &args); err != nil {
			return nil, apiErr(proto.CodeInvalidArgument, "", "invalid pause request")
		}
	}
	if args.Instance != "" && args.Instance != l.instance {
		return nil, apiErr(proto.CodeConflict, "state_changed", "the Worker restarted since its status was read; nothing was changed")
	}
	if !pause {
		st, err := l.pause.Resume(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(st)
	}
	if l.pause.dir != "" && !l.hubConfigured {
		return nil, apiErr(proto.CodeInvalidArgument, "", "no Hub configured: nothing to pause")
	}
	var until time.Time
	if args.Until != "" {
		t, err := time.Parse(time.RFC3339, args.Until)
		if err != nil {
			return nil, apiErr(proto.CodeInvalidArgument, "", fmt.Sprintf("invalid end of the pause %q", args.Until))
		}
		until = t
	}
	res, err := l.pause.Pause(ctx, until)
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}
