package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// enableDebugRemoteHelp is the part of every refusal that says how to allow
// remote debug on the Worker machine.
const enableDebugRemoteHelp = "On the Worker machine start it with `minion worker up --foreground --allow-debug-remote` " +
	"(add your usual flags), or for the service run `minion worker config set allow_debug_remote true` and restart it " +
	"(`minion worker up`, or `minion worker down` and `minion worker up`). MINION_ALLOW_DEBUG_REMOTE=1 works as well."

// maxLogsBytes bounds the JSON size of the lines one worker_logs answer
// carries, well below the stream frame limit.
const maxLogsBytes = 700 << 10

// debugRemote serves worker_debug and worker_logs (v0.10.0): the owner of the
// Worker or an administrator switches debug logging on for a while and reads
// the recent log, through the Hub, and only when this Worker allows it
// (allow_debug_remote, off by default).
type debugRemote struct {
	log      *slog.Logger
	allowed  bool
	live     *logx.Live
	workerID func() string
	now      func() time.Time
	// afterFunc runs f after d; the returned function cancels it (tests
	// replace it).
	afterFunc func(d time.Duration, f func()) (stop func() bool)

	mu     sync.Mutex
	until  time.Time
	cancel func() bool
}

// setupDebugRemote reads allow_debug_remote and registers the two ops on the
// executor. Without a runtime log control (tests, an embedded Worker) both ops
// answer that remote debug is not available.
func setupDebugRemote(log *slog.Logger, s Settings, ex *Executor) *debugRemote {
	d := &debugRemote{log: log, live: s.LogLive, now: time.Now, afterFunc: realAfterFunc}
	if p := ex.Policy(); p != nil {
		d.workerID = p.WorkerID
	}
	if s.Resolver != nil {
		on, err := config.ResolveBool(s.Resolver, config.KeyAllowDebugRemote)
		if err != nil {
			log.Warn("remote debug stays off: the setting is invalid", "error", err.Error())
		}
		d.allowed = on && err == nil
	}
	log.Debug("remote debug", "allow_debug_remote", d.allowed, "log_control", d.live != nil)
	d.register(ex)
	return d
}

// withDebugFact adds whether remote debug is allowed to the hello facts, so
// that the Hub shows the Debug panel only where it works.
func withDebugFact(facts func() transport.WorkerFacts, d *debugRemote) func() transport.WorkerFacts {
	return func() transport.WorkerFacts {
		f := facts()
		f.DebugRemote = d.Allowed()
		return f
	}
}

func realAfterFunc(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }

func (d *debugRemote) register(e *Executor) {
	e.Handle(ops.OpWorkerDebug, d.handleDebug)
	e.Handle(ops.OpWorkerLogs, d.handleLogs)
}

// Allowed reports whether this Worker serves remote debug (the hello fact).
func (d *debugRemote) Allowed() bool { return d.allowed && d.live != nil }

// check admits only a request the Hub routed to this Worker, and only when the
// Worker allows remote debug.
func (d *debugRemote) check(rq Request, workerID string) error {
	if rq.Origin == nil {
		return apiErr(proto.CodeForbidden, "", "remote debug is run through the Hub; on this machine use `minion worker up --foreground --debug` instead")
	}
	if d.workerID == nil || workerID == "" || workerID != d.workerID() {
		return apiErr(proto.CodeForbidden, "", "worker_id names another Worker")
	}
	if !d.allowed {
		return apiErr(proto.CodeForbidden, "", "this Worker does not allow remote debug (allow_debug_remote is off). "+enableDebugRemoteHelp)
	}
	if d.live == nil {
		return apiErr(proto.CodeForbidden, "", "this Worker runs without a switchable log, so remote debug is not available")
	}
	return nil
}

func (d *debugRemote) handleDebug(_ context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.WorkerDebugArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := d.check(rq, args.WorkerID); err != nil {
		return nil, err
	}
	if !args.Enabled {
		d.set(false, 0)
		d.log.Info("debug logging switched off through the Hub")
		return d.state(), nil
	}
	minutes := args.Minutes
	switch {
	case minutes == 0:
		minutes = ops.DefaultDebugMinutes
	case minutes < 0 || minutes > ops.MaxDebugMinutes:
		return nil, apiErr(proto.CodeInvalidArgument, "minutes", "minutes must be 1 to 1440")
	}
	d.set(true, time.Duration(minutes)*time.Minute)
	st := d.state()
	d.log.Info("debug logging switched on through the Hub", "minutes", minutes)
	return st, nil
}

// set switches debug logging; on keeps it for dur, then it switches itself off.
func (d *debugRemote) set(on bool, dur time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	d.until = time.Time{}
	d.live.SetDebug(on)
	if !on {
		return
	}
	d.until = d.now().Add(dur)
	until := d.until
	d.cancel = d.afterFunc(dur, func() { d.expire(until) })
}

// expire ends the remote debug that was switched on until until, unless a
// later request replaced it.
func (d *debugRemote) expire(until time.Time) {
	d.mu.Lock()
	if !d.until.Equal(until) {
		d.mu.Unlock()
		return
	}
	d.until, d.cancel = time.Time{}, nil
	d.live.SetDebug(false)
	d.mu.Unlock()
	d.log.Info("remote debug logging ended (its time was up)")
}

func (d *debugRemote) state() ops.WorkerDebugState {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := ops.WorkerDebugState{Debug: d.live.Debug(), Level: strings.ToLower(d.live.Level.Level().String())}
	if !d.until.IsZero() {
		at := ops.Timestamp(d.until)
		st.Remote, st.Until = true, &at
		if left := d.until.Sub(d.now()); left > 0 {
			st.RemainingSeconds = int(left / time.Second)
		}
	}
	return st
}

func (d *debugRemote) handleLogs(_ context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.WorkerLogsArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := d.check(rq, args.WorkerID); err != nil {
		return nil, err
	}
	n := args.Lines
	switch {
	case n == 0:
		n = ops.DefaultLogLines
	case n < 0 || n > ops.MaxLogLines:
		return nil, apiErr(proto.CodeInvalidArgument, "lines", "lines must be 1 to 2000")
	}
	lines := d.live.Ring.Tail(n)
	// Keep the newest lines that fit into one answer.
	size, from := 0, len(lines)
	for from > 0 {
		b, _ := json.Marshal(lines[from-1])
		if size+len(b)+1 > maxLogsBytes {
			break
		}
		size += len(b) + 1
		from--
	}
	kept, _, dropped := d.live.Ring.Stats()
	return ops.WorkerLogsResult{WorkerDebugState: d.state(), Lines: append([]string{}, lines[from:]...), Kept: kept, Dropped: dropped}, nil
}

// errString is err's text, or "" for nil (for a log attribute).
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
