package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// Module name and start order of the local control socket. It starts after the
// core module, whose [Executor] it serves.
const (
	// ModuleLocalAPI serves the local control socket (AD-22).
	ModuleLocalAPI = "localapi"
	// OrderLocalAPI is the start order of the local socket module.
	OrderLocalAPI = 200
)

func init() {
	if err := RegisterLocalAPI(DefaultRegistry); err != nil {
		panic(err)
	}
}

// localAPIDeps are the seams of the module that tests replace.
type localAPIDeps struct {
	socketPath func() (string, error)
	listen     func(path string) (net.Listener, error)
}

func defaultLocalAPIDeps() localAPIDeps {
	return localAPIDeps{
		socketPath: func() (string, error) { return localapi.SocketPath(config.SystemPathEnv()) },
		listen:     localapi.Listen,
	}
}

// RegisterLocalAPI registers the local socket module on r. It requires the
// *Executor of the core module and serves it on <runtime dir>/worker.sock, so
// that the CLI, `mn` and Solo MCP work without any network listener (AD-22).
// A second Worker of the same user fails to start rather than take the socket
// over. On Windows the socket is the named pipe of the user (localapi.Listen).
// Where neither exists the module logs a warning and the Worker runs without a
// local socket.
func RegisterLocalAPI(r *Registry) error {
	return r.Register(ModuleLocalAPI, OrderLocalAPI, defaultLocalAPIDeps().start)
}

func (d localAPIDeps) start(_ context.Context, env *Env) (StopFunc, error) {
	ex, err := Require[*Executor](env)
	if err != nil {
		return nil, err
	}
	log := env.Settings().logger()

	path, err := d.socketPath()
	if err != nil {
		return nil, fmt.Errorf("find the local control socket path: %w", err)
	}
	l, err := d.listen(path)
	if errors.Is(err, localapi.ErrUnsupported) {
		log.Warn("the local control socket is not available on this system; the CLI cannot reach this Worker locally")
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// The connector is optional here: it starts earlier (same order, name
	// sorts first) in a real Worker, and a Worker without one reports an idle
	// connector. The pairing code has no source in the Worker yet, so the
	// status says "unknown" for it instead of "none".
	conn, _ := lookupService[*Connector](env.rt)
	live := LiveStatusSource{Connector: conn, Policy: ex.Policy()}
	// The pipe module starts before this one. Without it the pipe ops are
	// refused here and never reach the executor, whose pipe_send is the
	// Hub-origin write (exec_pipe.go).
	pipeSvc, _ := lookupService[*PipeService](env.rt)
	srv, err := localapi.New(localapi.Config{
		Executor: localExecutor{ex: ex, log: log, live: live, stateDir: env.Settings().StateDir, pipe: pipeSvc},
		Logger:   log,
	})
	if err != nil {
		_ = l.Close()
		return nil, err
	}
	go func() {
		if err := srv.Serve(l); err != nil {
			log.Error("local control socket stopped", "error", err)
		}
	}()
	log.Info("local control socket listening", "socket", path)
	return func(ctx context.Context) error { return srv.Close(ctx) }, nil
}

// localExecutor adapts the Worker [Executor] to the localapi port.
type localExecutor struct {
	ex  *Executor
	log *slog.Logger
	// live and stateDir answer OpLocalStatus, which is served here and never
	// reaches the Executor (it is not a catalog op).
	live     LiveSource
	stateDir string
	// pipe serves the pipe ops of a local call (Story 7.3); nil refuses them.
	pipe *PipeService
}

func (l localExecutor) Execute(ctx context.Context, rq localapi.Request, progress func(json.RawMessage)) (json.RawMessage, error) {
	// A capability-qualified request ("answer+answer.reviewed", see
	// localapi.QualifiedOp) states what the client relies on. It is checked here,
	// before anything is decided or done, against what this Worker really
	// implements; the suffix is then dropped. An older Worker never gets here: it
	// does not know the qualified name and refuses it as an unsupported operation.
	if base, need := localapi.SplitOp(rq.Op); len(need) > 0 {
		have := l.ex.Features()
		for _, f := range need {
			if !proto.HasFeature(have, f) {
				return nil, l.protoError(base, apiErr(proto.CodeUnsupportedOp, "", fmt.Sprintf("this Worker does not announce the feature %q that %s needs; nothing was sent", f, base)))
			}
		}
		rq.Op = base
	}
	if rq.Op == OpLocalStatus && l.live != nil {
		raw, err := statusOp(ctx, l.stateDir, l.live)
		if err != nil {
			return nil, l.protoError(rq.Op, err)
		}
		return raw, nil
	}
	ctx = WithProgress(ctx, progress)
	if IsPipeOp(rq.Op) {
		// A call from this host: the origin is this entry point, so the pipe
		// ops of the local socket never run as a Hub request, and the
		// executor's pipe_send (the Hub's write) is not reachable from here.
		if l.pipe == nil {
			return nil, l.protoError(rq.Op, apiErr(proto.CodeUnsupportedOp, "", "the pipe is not running on this Worker"))
		}
		raw, err := l.pipe.Local(ctx, Request{Op: rq.Op, Actions: rq.Actions, Args: rq.Args})
		if err != nil {
			return nil, l.protoError(rq.Op, err)
		}
		return raw, nil
	}
	raw, err := l.ex.Execute(ctx, Request{Op: rq.Op, Actions: rq.Actions, Args: rq.Args})
	if err != nil {
		return nil, l.protoError(rq.Op, err)
	}
	return raw, nil
}

// protoError maps an executor error to its wire form.
func (l localExecutor) protoError(op string, err error) error {
	if ae, ok := AsAPIError(err); ok {
		// The cause of an internal failure never reaches the caller, and the
		// executor does not log it: record it here, with the op and the code
		// only, never arguments, results or terminal content (AD-20).
		if ae.Code == proto.CodeInternal && ae.Err != nil {
			l.log.Error("local op failed", "op", op, "code", string(ae.Code), "error", ae.Err)
		}
		return ae.ProtoError()
	}
	return err
}

// ProtoError returns the wire form of the error: the code, the message and,
// when there is one, the more specific reason as details {"reason": ...}. The
// cause is never included.
func (e *APIError) ProtoError() *proto.Error {
	pe := proto.NewError(e.Code, e.Message)
	if len(e.Details) > 0 {
		pe.Details = e.Details
	} else if e.Reason != "" {
		pe.Details, _ = json.Marshal(map[string]string{"reason": string(e.Reason)})
	}
	return pe
}

type progressKey struct{}

// WithProgress returns a context whose [EmitProgress] sends to fn. An entry
// point that can stream (the local socket, later the Hub op server) uses it so
// that a streaming handler, such as pipe_wait, delivers its elements as
// `op.progress` frames without knowing the entry point.
func WithProgress(ctx context.Context, fn func(json.RawMessage)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, progressKey{}, fn)
}

// EmitProgress sends v, encoded as JSON, as one `op.progress` element of the
// current request. It does nothing when the entry point cannot stream.
func EmitProgress(ctx context.Context, v any) error {
	fn, _ := ctx.Value(progressKey{}).(func(json.RawMessage))
	if fn == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	fn(b)
	return nil
}
