// Package devctltest is a fake of the app's adb bridge socket for tests. It
// speaks the bridge protocol of package devctl (docs/minion-android-adb/
// bridge-protocol.md) and never touches a device.
package devctltest

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/worker/devctl"
)

// Result is what the fake answers to one shell, screencap or input request.
type Result struct {
	Stdout, Stderr []byte
	Code           int
	// Err is an adb.error code instead of an exit.
	Err string
	// Block makes the request wait until the Worker closes the connection or
	// the fake is disabled.
	Block bool
}

// Bridge is a fake bridge on a Unix socket.
type Bridge struct {
	Path  string
	Token string

	mu      sync.Mutex
	status  devctl.Status
	handler func(op, command string) Result
	reqs    []devctl.Request
	streams map[net.Conn]chan struct{}
	l       net.Listener
	wg      sync.WaitGroup
}

// Start starts a fake bridge in dir with token and an initial status: feature
// on, enabled, READY on port 37000 with a Keystore key.
func Start(dir, token string) (*Bridge, error) {
	path := filepath.Join(dir, devctl.SocketName)
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	b := &Bridge{Path: path, Token: token, l: l, streams: map[net.Conn]chan struct{}{},
		status:  devctl.Status{FeatureEnabled: true, Enabled: true, State: devctl.StateReady, Port: 37000, KeyStorage: "android-keystore (TEE)"},
		handler: func(string, string) Result { return Result{} },
	}
	b.wg.Add(1)
	go b.serve()
	return b, nil
}

// Close stops the fake.
func (b *Bridge) Close() {
	_ = b.l.Close()
	b.mu.Lock()
	for c, ch := range b.streams {
		close(ch)
		delete(b.streams, c)
	}
	b.mu.Unlock()
	b.wg.Wait()
}

// SetStatus replaces the status the fake reports.
func (b *Bridge) SetStatus(st devctl.Status) {
	b.mu.Lock()
	b.status = st
	b.mu.Unlock()
}

// Status returns the status the fake reports now.
func (b *Bridge) Status() devctl.Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

// Handle sets the answer of shell, screencap and input requests.
func (b *Bridge) Handle(fn func(op, command string) Result) {
	b.mu.Lock()
	b.handler = fn
	b.mu.Unlock()
}

// Requests returns the requests received so far (tokens included: the fake is
// the only place where a test may look at one).
func (b *Bridge) Requests() []devctl.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]devctl.Request(nil), b.reqs...)
}

// Ops returns the ops of the requests received so far.
func (b *Bridge) Ops() []string {
	var out []string
	for _, r := range b.Requests() {
		out = append(out, r.Op)
	}
	return out
}

// Active is the number of open streams.
func (b *Bridge) Active() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.streams)
}

func (b *Bridge) serve() {
	defer b.wg.Done()
	for {
		c, err := b.l.Accept()
		if err != nil {
			return
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer func() { _ = c.Close() }()
			b.conn(c)
		}()
	}
}

func fail(c net.Conn, code string) {
	_ = devctl.WriteFrame(c, devctl.ChannelControl, devctl.ErrorFrame{Type: devctl.TypeError, Code: code})
}

func (b *Bridge) conn(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	rq, err := devctl.ReadRequest(c)
	if err != nil {
		fail(c, devctl.ErrBadRequest)
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	b.mu.Lock()
	b.reqs = append(b.reqs, rq)
	st, h := b.status, b.handler
	b.mu.Unlock()
	if rq.Version != devctl.ProtocolVersion {
		fail(c, devctl.ErrBadRequest)
		return
	}
	if rq.Token != b.Token {
		fail(c, devctl.ErrBadToken)
		return
	}
	switch rq.Op {
	case devctl.OpStatus:
		st.Type = devctl.TypeStatus
		b.mu.Lock()
		st.ActiveStreams = len(b.streams)
		b.mu.Unlock()
		_ = devctl.WriteFrame(c, devctl.ChannelControl, st)
		return
	case devctl.OpDisable:
		b.mu.Lock()
		b.status.Enabled = false
		b.status.State = devctl.StateOff
		for sc, ch := range b.streams {
			close(ch)
			delete(b.streams, sc)
		}
		b.mu.Unlock()
		_ = devctl.WriteFrame(c, devctl.ChannelControl, map[string]string{"type": devctl.TypeOK})
		return
	case devctl.OpShell, devctl.OpScreencap, devctl.OpInput:
	default:
		fail(c, devctl.ErrBadRequest)
		return
	}
	// The real bridge checks the policy of its own side too.
	if !st.FeatureEnabled {
		fail(c, devctl.ErrNotAvailable)
		return
	}
	if !st.Enabled {
		fail(c, devctl.ErrDisabled)
		return
	}
	if st.State != devctl.StateReady && st.State != devctl.StatePaired {
		fail(c, map[string]string{
			devctl.StateWaitingWiFi: devctl.ErrNeedsWiFi, devctl.StateEndpointUnverified: devctl.ErrEndpointUnverified,
		}[st.State])
		return
	}
	res := h(rq.Op, rq.Command)
	if res.Err == "" {
		_ = devctl.WriteFrame(c, devctl.ChannelControl, map[string]string{"type": devctl.TypeStarted})
	}
	if res.Block {
		ch := make(chan struct{})
		b.mu.Lock()
		b.streams[c] = ch
		b.mu.Unlock()
		gone := make(chan struct{})
		go func() {
			var one [1]byte
			_, _ = c.Read(one[:])
			close(gone)
		}()
		timeout := time.Duration(rq.TimeoutMS) * time.Millisecond
		if timeout <= 0 {
			timeout = time.Hour
		}
		select {
		case <-ch:
			fail(c, devctl.ErrDisabled)
		case <-gone:
		case <-time.After(timeout):
			fail(c, devctl.ErrTimeout)
		}
		b.mu.Lock()
		delete(b.streams, c)
		b.mu.Unlock()
		return
	}
	if res.Err != "" {
		fail(c, res.Err)
		return
	}
	if err := sendChunks(c, devctl.ChannelStdout, res.Stdout); err != nil {
		return
	}
	if err := sendChunks(c, devctl.ChannelStderr, res.Stderr); err != nil {
		return
	}
	_ = devctl.WriteFrame(c, devctl.ChannelControl, devctl.Exit{Type: devctl.TypeExit, Code: res.Code})
}

func sendChunks(c net.Conn, ch byte, b []byte) error {
	for len(b) > 0 {
		n := min(len(b), devctl.MaxPayload)
		if err := devctl.WriteFrame(c, ch, b[:n]); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// WaitActive waits until n streams are open or ctx ends.
func (b *Bridge) WaitActive(ctx context.Context, n int) error {
	for b.Active() != n {
		select {
		case <-ctx.Done():
			return errors.New("devctltest: streams did not reach the count")
		case <-time.After(5 * time.Millisecond):
		}
	}
	return nil
}
