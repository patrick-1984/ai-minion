package devctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// ConnectTimeout bounds dialling the bridge and the bridge's own connect to
// adbd before the first frame: "no hang" (DESIGN 4.6).
const ConnectTimeout = 5 * time.Second

// Command timeouts of shell, screencap and input (DESIGN 4.6).
const (
	DefaultTimeout = 120 * time.Second
	MaxTimeout     = 3600 * time.Second
)

// Client talks to the bridge socket. The zero value is not usable; build it
// with [NewClient].
type Client struct {
	path  string
	token func() string
	// dial is replaced by tests.
	dial func(ctx context.Context, path string) (net.Conn, error)
}

// NewClient returns a client of the bridge socket at path. token returns the
// bridge token of this start ("" when the app gave none).
func NewClient(path string, token func() string) *Client {
	if token == nil {
		token = func() string { return "" }
	}
	return &Client{path: path, token: token, dial: func(ctx context.Context, p string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", p)
	}}
}

// Path is the bridge socket path.
func (c *Client) Path() string { return c.path }

func (c *Client) open(ctx context.Context, rq Request) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	conn, err := c.dial(dctx, c.path)
	if err != nil {
		// No socket, nobody listening, or no answer in time: the app's
		// bridge is not running. The cause is not more useful to a caller.
		return nil, &Error{Code: ErrBridgeDown}
	}
	rq.Version, rq.Token = ProtocolVersion, c.token()
	_ = conn.SetWriteDeadline(time.Now().Add(ConnectTimeout))
	if err := WriteFrame(conn, ChannelControl, rq); err != nil {
		_ = conn.Close()
		return nil, &Error{Code: ErrBridgeDown}
	}
	_ = conn.SetWriteDeadline(time.Time{})
	return conn, nil
}

// Status asks the bridge for its status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	conn, err := c.open(ctx, Request{Op: OpStatus})
	if err != nil {
		return Status{}, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(ConnectTimeout))
	typ, body, err := readControl(conn)
	if err != nil {
		return Status{}, err
	}
	switch typ {
	case TypeStatus:
		var st Status
		if err := json.Unmarshal(body, &st); err != nil {
			return Status{}, &Error{Code: ErrInternal, Message: "the bridge sent an unreadable status"}
		}
		st.Type = ""
		return st, nil
	case TypeError:
		return Status{}, decodeError(body)
	}
	return Status{}, &Error{Code: ErrInternal, Message: fmt.Sprintf("unexpected bridge frame %q", typ)}
}

// Disable asks the bridge to disable device control and close every stream.
func (c *Client) Disable(ctx context.Context, reason string) error {
	conn, err := c.open(ctx, Request{Op: OpDisable, Reason: reason})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(ConnectTimeout))
	typ, body, err := readControl(conn)
	if err != nil {
		return err
	}
	switch typ {
	case TypeOK:
		return nil
	case TypeError:
		return decodeError(body)
	}
	return &Error{Code: ErrInternal, Message: fmt.Sprintf("unexpected bridge frame %q", typ)}
}

// Sink receives the data of a running operation. It must not keep b.
type Sink func(stderr bool, b []byte) error

// Run runs shell, screencap or input and streams its data to sink. It returns
// the remote exit code. ctx ending (the caller left, the timeout, a disable)
// closes the stream. timeout is clamped to [1s, MaxTimeout]; 0 means
// [DefaultTimeout].
func (c *Client) Run(ctx context.Context, op, command string, timeout time.Duration, sink Sink) (int, error) {
	switch op {
	case OpShell, OpScreencap, OpInput:
	default:
		return 0, &Error{Code: ErrBadRequest, Message: "unknown device control operation"}
	}
	timeout = ClampTimeout(timeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := c.open(ctx, Request{Op: op, Command: command, TimeoutMS: timeout.Milliseconds()})
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	for {
		ch, body, err := ReadFrame(conn, MaxPayload)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return 0, &Error{Code: ErrTimeout}
			}
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, &Error{Code: ErrBridgeDown, Message: "the device control bridge closed the stream"}
		}
		if ch != ChannelControl {
			if sink != nil {
				if err := sink(ch == ChannelStderr, body); err != nil {
					return 0, err
				}
			}
			continue
		}
		typ, err := ControlType(body)
		if err != nil {
			return 0, &Error{Code: ErrInternal, Message: "the bridge sent an unreadable frame"}
		}
		switch typ {
		case TypeStarted:
		case TypeExit:
			var e Exit
			if err := json.Unmarshal(body, &e); err != nil {
				return 0, &Error{Code: ErrInternal, Message: "the bridge sent an unreadable exit"}
			}
			return e.Code, nil
		case TypeError:
			return 0, decodeError(body)
		default:
			return 0, &Error{Code: ErrInternal, Message: fmt.Sprintf("unexpected bridge frame %q", typ)}
		}
	}
}

// readControl reads the one control frame of a status or disable answer.
func readControl(conn net.Conn) (string, []byte, error) {
	ch, body, err := ReadFrame(conn, MaxPayload)
	if err != nil {
		return "", nil, &Error{Code: ErrBridgeDown}
	}
	if ch != ChannelControl {
		return "", nil, &Error{Code: ErrInternal, Message: "the bridge sent data where a control frame was due"}
	}
	typ, err := ControlType(body)
	if err != nil {
		return "", nil, &Error{Code: ErrInternal, Message: "the bridge sent an unreadable frame"}
	}
	return typ, body, nil
}

// ClampTimeout applies the default and the bounds of a command timeout.
func ClampTimeout(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return DefaultTimeout
	case d < time.Second:
		return time.Second
	case d > MaxTimeout:
		return MaxTimeout
	}
	return d
}

func decodeError(body []byte) error {
	var f ErrorFrame
	if err := json.Unmarshal(body, &f); err != nil || f.Code == "" {
		return &Error{Code: ErrInternal}
	}
	// The bridge's message is shown to the caller; it must be a short class
	// text. Unknown codes keep the message but count as internal.
	return &Error{Code: f.Code, Message: f.Message}
}
