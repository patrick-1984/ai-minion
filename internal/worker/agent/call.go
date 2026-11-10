package agent

// Story 7.4: the call client (AD-3, AD-11, AD-22).
//
// In Hub mode a command that runs on a Worker host but acts on another
// Worker's session, for example `mn pipe_send` inside a session, does not
// reach the target through a local socket: the Worker forwards it to the Hub
// on a "ca" (call) stream and the Hub routes it. [CallClient] is that
// forwarder. It serves every welcomed [Link] like the op server does
// ([Connector.OnLink]) and remembers the current one, so a caller (the local
// socket handler, the MCP server on a Worker host) only calls [CallClient.Do]
// or [CallClient.Stream]; with no Hub connection both fail at once with
// worker_offline and nothing is queued.
//
// One call is one stream: the client opens it, writes exactly one op.request,
// then reads op.progress frames and one op.result or error frame. The client
// keeps the stream open until it has the answer, because the Hub reads the
// end of the stream as "the caller is gone". With an API key (usually a
// Worker-bound key read from the key file, [CallConfig.Key]) the request
// carries it as api_key and the Hub authenticates that key, which must be
// bound to this Worker; without one the Hub speaks for the Worker identity of
// the connection. The key is never logged and never part of an error.
//
// A write is never retried here (AD-15). If the connection is lost after the
// request was sent, the error is a [*CallLostError] (code worker_offline): the
// Hub may or may not have applied the write, and the caller must not repeat
// it blindly.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// DefaultCallTimeout bounds a unary call from the first byte to the answer.
// A streaming call is bounded by its caller's context. It is longer than the
// two minutes the Hub's router waits for a Worker's answer to a write, so that
// a Hub that answers "unknown" at its own deadline is still heard; if this
// timer does fire after the request was sent, the outcome of a write is
// unknown, like a lost connection (see [CallLostError]).
const DefaultCallTimeout = 2*time.Minute + 30*time.Second

// CallConfig configures a [CallClient].
type CallConfig struct {
	// Key returns the API key to present, or "" to speak as the Worker
	// identity. It is called for every call, so a rotated key file takes
	// effect at once. Nil means no key.
	Key func() (string, error)
	// Timeout bounds a unary call; the default is [DefaultCallTimeout].
	Timeout time.Duration
	// WriteTimeout bounds one frame write; the default is [DefaultWriteTimeout].
	WriteTimeout time.Duration
}

// CallClient forwards ops to the Hub on call streams.
type CallClient struct {
	cfg CallConfig

	mu  sync.Mutex
	cur *Link
}

// NewCallClient returns a call client. Register [CallClient.Serve] with
// [Connector.OnLink] to give it the connection.
func NewCallClient(cfg CallConfig) *CallClient {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultCallTimeout
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	return &CallClient{cfg: cfg}
}

// Serve is the [LinkHandler] of the client: it makes l the current link until
// l ends.
func (c *CallClient) Serve(l *Link) {
	c.mu.Lock()
	c.cur = l
	c.mu.Unlock()
	<-l.Context().Done()
	c.mu.Lock()
	if c.cur == l {
		c.cur = nil
	}
	c.mu.Unlock()
}

// Connected reports whether a Hub connection is up.
func (c *CallClient) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur != nil
}

func (c *CallClient) link() (*Link, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur == nil || c.cur.Context().Err() != nil {
		return nil, proto.NewError(proto.CodeWorkerOffline, "this Worker is not connected to a Hub")
	}
	return c.cur, nil
}

// CallLostError says that the call ended after the request was sent and before
// the answer came: the Hub may have applied it. It is returned for every way a
// write ends there: the connection was lost, the Hub's answer was unreadable,
// the call's own timeout ran out, or the caller's context ended (the cause says
// which, and errors.Is finds it). A failure before the request was written is
// never a CallLostError: nothing was sent and the request may be made again.
type CallLostError struct{ Cause error }

// Error implements error.
func (e *CallLostError) Error() string {
	return "the connection to the Hub ended before it answered; the request may have been applied"
}

// Unwrap returns the cause.
func (e *CallLostError) Unwrap() error { return e.Cause }

// Is matches any *proto.Error with the code worker_offline, which is the code
// of this error.
func (e *CallLostError) Is(target error) bool {
	var t *proto.Error
	return errors.As(target, &t) && t != nil && t.Code == proto.CodeWorkerOffline
}

// As lets errors.As and proto.CodeOf see the *proto.Error behind it.
func (e *CallLostError) As(target any) bool {
	if t, ok := target.(**proto.Error); ok {
		*t = proto.NewError(proto.CodeWorkerOffline, e.Error())
		return true
	}
	return false
}

// callFrame is the op.request frame of a call stream.
type callFrame struct {
	Type      string          `json:"type"`
	Op        string          `json:"op"`
	RequestID string          `json:"request_id"`
	Args      json.RawMessage `json:"args"`
	APIKey    string          `json:"api_key,omitempty"`
}

// Do runs op on the Hub and decodes the result into out (a pointer; nil
// discards it). args is the op's argument struct or map. A Hub error frame is
// returned as a *proto.Error.
func (c *CallClient) Do(ctx context.Context, op string, args, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	var raw json.RawMessage
	err := c.run(ctx, op, args, nil, func(body json.RawMessage) { raw = body })
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return proto.NewError(proto.CodeInternal, "the Hub sent a result this Worker cannot read")
	}
	return nil
}

// Stream runs a streaming op on the Hub. onElement gets the data of every
// op.progress frame in order; an error from it abandons the call and is
// returned. The call ends with the Hub's closing op.result, an error frame or
// ctx.
func (c *CallClient) Stream(ctx context.Context, op string, args any, onElement func(json.RawMessage) error) error {
	return c.run(ctx, op, args, onElement, func(json.RawMessage) {})
}

func (c *CallClient) run(ctx context.Context, op string, args any, onElement func(json.RawMessage) error, onResult func(json.RawMessage)) error {
	l, err := c.link()
	if err != nil {
		return err
	}
	req, err := c.request(op, args)
	if err != nil {
		return err
	}
	// Whatever can be wrong with the frame itself (it does not encode, it is too
	// large) is found here, before a stream is opened and before a byte is
	// written: that is the only kind of failure that proves nothing was sent. An
	// error of the write below is a failure of the connection, which may come
	// after the whole frame has reached the Hub.
	if err := proto.WriteFrame(io.Discard, proto.StreamFrameLimit, req); err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) {
			return pe
		}
		return proto.NewError(proto.CodeInvalidArgument, "the call cannot be encoded")
	}
	st, err := l.Session.OpenCall(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return proto.NewError(proto.CodeWorkerOffline, "the connection to the Hub could not take a new call")
	}
	// A context that ends abandons the stream, which ends the blocked read.
	stop := context.AfterFunc(ctx, st.Reset)
	defer stop()

	write := isWriteOp(op)
	_ = st.SetWriteDeadline(time.Now().Add(c.cfg.WriteTimeout))
	if err := st.WriteFrame(req); err != nil {
		st.Reset()
		if write {
			// The write of the frame failed on the connection: its bytes may
			// all have reached the Hub before the error (a write that completes
			// late, a deadline of the connection, a reset after the last byte).
			// The request may have been applied, so the outcome is unknown and
			// the caller must not send it again under another id.
			return &CallLostError{Cause: err}
		}
		return proto.NewError(proto.CodeWorkerOffline, "the call could not be sent to the Hub")
	}
	_ = st.SetWriteDeadline(time.Time{})
	// From here on the request is out. For a write, whatever ends the call
	// before its answer, the caller's cancellation and the call's own deadline
	// included, leaves the outcome unknown; for a read the context error is the
	// plain answer it always was.
	lost := func(cause error) error {
		st.Reset()
		if ctx.Err() != nil {
			if !write {
				return ctx.Err()
			}
			cause = ctx.Err()
		}
		return &CallLostError{Cause: cause}
	}
	for {
		body, err := readFrameCtx(ctx, st)
		if err != nil {
			return lost(err)
		}
		typ, err := proto.FrameType(body)
		if err != nil {
			return lost(err)
		}
		switch typ {
		case proto.TypeOpProgress:
			if onElement == nil {
				continue
			}
			var f struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(body, &f); err != nil {
				return lost(err)
			}
			if err := onElement(f.Data); err != nil {
				st.Reset()
				return err
			}
		case proto.TypeOpResult:
			var f struct {
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(body, &f); err != nil {
				return lost(err)
			}
			_ = st.Close()
			onResult(f.Result)
			return nil
		case proto.TypeError:
			var f proto.ErrorFrame
			if err := json.Unmarshal(body, &f); err != nil {
				return lost(err)
			}
			_ = st.Close()
			return f.Err()
		}
		// Any other frame type is ignored: minor protocol changes are additive.
	}
}

// isWriteOp reports whether op changes state (a catalog write or an op that
// mutates), whose outcome is unknown when
// the call ends after the request was sent.
func isWriteOp(op string) bool {
	o, ok := ops.Lookup(op)
	return ok && o.ChangesState()
}

// request builds the op.request frame, including the API key if one is set.
func (c *CallClient) request(op string, args any) (callFrame, error) {
	if op == "" {
		return callFrame{}, proto.NewError(proto.CodeInvalidArgument, "an operation name is required")
	}
	body := json.RawMessage("{}")
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return callFrame{}, proto.NewError(proto.CodeInvalidArgument, "the arguments cannot be encoded")
		}
		body = b
	}
	f := callFrame{Type: proto.TypeOpRequest, Op: op, RequestID: ops.NewUUIDv7(), Args: body}
	if c.cfg.Key != nil {
		k, err := c.cfg.Key()
		if err != nil {
			// The error of a key source can quote a path, never the key.
			return callFrame{}, proto.NewError(proto.CodeUnauthenticated, fmt.Sprintf("the API key cannot be read: %v", err))
		}
		f.APIKey = k
	}
	return f, nil
}

// readFrameCtx reads one frame and gives up when ctx ends. Resetting a
// transport stream half-closes it and drains the rest, so a read in flight can
// stay blocked on a silent Hub for the stream close timeout; the caller must
// not wait for it. The read still ends once the stream is gone.
func readFrameCtx(ctx context.Context, st *transport.Stream) (json.RawMessage, error) {
	type result struct {
		body json.RawMessage
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		body, err := st.ReadFrame()
		ch <- result{body, err}
	}()
	select {
	case r := <-ch:
		return r.body, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
